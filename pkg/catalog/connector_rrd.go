package connector

import (
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/facette/facette/pkg/types"
	"github.com/facette/facette/pkg/utils"
	"github.com/facette/facette/thirdparty/github.com/ziutek/rrd"
)

// RRDConnectorHandler represents the main structure of the RRD connector.
type RRDConnectorHandler struct {
	Path    string
	Pattern string
	origin  *Origin
	metrics map[string]map[string]*RRDMetric
}

// RRDMetric represents the main structure of a RRD metric information.
type RRDMetric struct {
	Dataset  string
	FilePath string
}

// GetPlots calculates and returns plot data based on a time interval.
func (handler *RRDConnectorHandler) GetPlots(query *GroupQuery, startTime, endTime time.Time, step time.Duration,
	percentiles []float64) (map[string]*PlotResult, error) {

	return handler.rrdGetData(query, startTime, endTime, step, percentiles, false)
}

// GetValue calculates and returns plot data at a specific reference time.
func (handler *RRDConnectorHandler) GetValue(query *GroupQuery, refTime time.Time,
	percentiles []float64) (map[string]map[string]types.PlotValue, error) {

	result := make(map[string]map[string]types.PlotValue)

	data, err := handler.rrdGetData(query, refTime.Add(-1*time.Minute), refTime, time.Minute, percentiles, true)
	if err != nil {
		return nil, err
	}

	for serieName := range data {
		result[serieName] = data[serieName].Info
	}

	return result, err
}

// Update triggers a full connector data update.
func (handler *RRDConnectorHandler) Update() error {
	// Compile pattern
	re := regexp.MustCompile(handler.Pattern)

	// Validate pattern keywords
	groups := make(map[string]bool)

	for _, key := range re.SubexpNames() {
		if key == "" {
			continue
		} else if key == "source" || key == "metric" {
			groups[key] = true
		} else {
			return fmt.Errorf("invalid pattern keyword `%s'", key)
		}
	}

	if !groups["source"] {
		return fmt.Errorf("missing pattern keyword `source'")
	} else if !groups["metric"] {
		return fmt.Errorf("missing pattern keyword `metric'")
	}

	// Search for files and parse their path for source/metric pairs
	walkFunc := func(filePath string, fileInfo os.FileInfo, err error) error {
		var (
			metric string
			source string
		)

		// Stop if previous error
		if err != nil {
			return err
		}

		// Skip non-files
		mode := fileInfo.Mode() & os.ModeType
		if mode != 0 {
			return nil
		}

		submatch := re.FindStringSubmatch(filePath[len(handler.Path)+1:])
		if len(submatch) == 0 {
			log.Printf("WARNING: file `%s' does not match pattern", filePath)
			return nil
		}

		if re.SubexpNames()[1] == "source" {
			source = submatch[1]
			metric = submatch[2]
		} else {
			source = submatch[2]
			metric = submatch[1]
		}

		if _, ok := handler.metrics[source]; !ok {
			handler.metrics[source] = make(map[string]*RRDMetric)
		}

		// Extract metric information from .rrd file
		info, err := rrd.Info(filePath)
		if err != nil {
			return err
		}

		if _, ok := info["ds.index"]; ok {
			for dsName := range info["ds.index"].(map[string]interface{}) {
				metricName := metric + "/" + dsName

				handler.origin.inputChan <- [2]string{source, metricName}
				handler.metrics[source][metricName] = &RRDMetric{Dataset: dsName, FilePath: filePath}
			}
		}

		return err
	}

	err := utils.WalkDir(handler.Path, walkFunc)
	if err != nil {
		return err
	}

	// Close channel once updated
	close(handler.origin.inputChan)

	return nil
}

func (handler *RRDConnectorHandler) rrdGetData(query *GroupQuery, startTime, endTime time.Time, step time.Duration,
	percentiles []float64, infoOnly bool) (map[string]*PlotResult, error) {

	var xport *rrd.Exporter

	if len(query.Series) == 0 {
		return nil, fmt.Errorf("group has no series")
	} else if query.Type != OperGroupTypeNone && len(query.Series) == 1 {
		query.Type = OperGroupTypeNone
	}

	result := make(map[string]*PlotResult)
	series := make(map[string]string)

	stack := []string{}
	graph := rrd.NewGrapher()

	if !infoOnly {
		xport = rrd.NewExporter()
	}

	count := 0

	switch query.Type {
	case OperGroupTypeNone:
		for _, serie := range query.Series {
			if serie.Metric == nil {
				continue
			}

			serieTemp := fmt.Sprintf("serie%d", count)
			serieName := serie.Name

			count += 1

			graph.Def(
				serieTemp+"-orig0",
				handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].FilePath,
				handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].Dataset,
				"AVERAGE",
			)

			if serie.Scale != 0 {
				graph.CDef(serieTemp+"-orig1", fmt.Sprintf("%s-orig0,%f,*", serieTemp, serie.Scale))
			} else {
				graph.CDef(serieTemp+"-orig1", serieTemp+"-orig0")
			}

			if query.Scale != 0 {
				graph.CDef(serieTemp, fmt.Sprintf("%s-orig1,%f,*", serieTemp, query.Scale))
			} else {
				graph.CDef(serieTemp, serieTemp+"-orig1")
			}

			// Set graph information request
			rrdSetGraph(graph, serieTemp, serieName, percentiles)

			// Set plots request
			if !infoOnly {
				xport.Def(
					serieTemp+"-orig0",
					handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].FilePath,
					handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].Dataset,
					"AVERAGE",
				)

				if serie.Scale != 0 {
					xport.CDef(serieTemp+"-orig1", fmt.Sprintf("%s-orig0,%f,*", serieTemp, serie.Scale))
				} else {
					xport.CDef(serieTemp+"-orig1", serieTemp+"-orig0")
				}

				if query.Scale != 0 {
					xport.CDef(serieTemp, fmt.Sprintf("%s-orig1,%f,*", serieTemp, query.Scale))
				} else {
					xport.CDef(serieTemp, serieTemp+"-orig1")
				}

				xport.XportDef(serieTemp, serieTemp)
			}

			// Set serie matching
			series[serieTemp] = serieName
		}

		break

	case OperGroupTypeAvg, OperGroupTypeSum:
		serieName := fmt.Sprintf("serie%d", count)
		count += 1

		for index, serie := range query.Series {
			if serie.Metric == nil {
				continue
			}

			serieTemp := serieName + fmt.Sprintf("-tmp%d", index)

			graph.Def(
				serieTemp,
				handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].FilePath,
				handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].Dataset,
				"AVERAGE",
			)

			if !infoOnly {
				xport.Def(
					serieTemp,
					handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].FilePath,
					handler.metrics[serie.Metric.source.Name][serie.Metric.OriginalName].Dataset,
					"AVERAGE",
				)
			}

			if len(stack) == 0 {
				stack = append(stack, serieTemp)
			} else {
				stack = append(stack, serieTemp, "+")
			}
		}

		if query.Type == OperGroupTypeAvg {
			stack = append(stack, strconv.Itoa(len(query.Series)), "/")
		}

		graph.CDef(serieName+"-orig", strings.Join(stack, ","))

		if query.Scale != 0 {
			graph.CDef(serieName, fmt.Sprintf("%s-orig,%f,*", serieName, query.Scale))
		} else {
			graph.CDef(serieName, serieName+"-orig")
		}

		// Set graph information request
		rrdSetGraph(graph, serieName, query.Name, percentiles)

		// Set plots request
		if !infoOnly {
			xport.CDef(serieName+"-orig", strings.Join(stack, ","))

			if query.Scale != 0 {
				xport.CDef(serieName, fmt.Sprintf("%s-orig,%f,*", serieName, query.Scale))
			} else {
				xport.CDef(serieName, serieName+"-orig")
			}

			xport.XportDef(serieName, serieName)
		}

		// Set serie matching
		series[serieName] = query.Name

		break

	default:
		return nil, fmt.Errorf("unknown `%d' operator type", query.Type)
	}

	// Get plots
	data := rrd.XportResult{}

	if !infoOnly {
		data, err := xport.Xport(startTime, endTime, step)
		if err != nil {
			return nil, err
		}

		for index, serieName := range data.Legends {
			result[series[serieName]] = &PlotResult{Info: make(map[string]types.PlotValue)}

			for i := 0; i < data.RowCnt; i++ {
				result[series[serieName]].Plots = append(result[series[serieName]].Plots,
					types.PlotValue(data.ValueAt(index, i)))
			}
		}
	}

	// Parse graph information
	graphInfo, _, err := graph.Graph(startTime, endTime)
	if err != nil {
		return nil, err
	}

	rrdParseInfo(graphInfo, result)

	data.FreeValues()

	return result, nil
}

func rrdParseInfo(info rrd.GraphInfo, data map[string]*PlotResult) {
	for _, value := range info.Print {
		chunks := strings.SplitN(value, ",", 3)

		chunkFloat, err := strconv.ParseFloat(chunks[2], 64)
		if err != nil {
			chunkFloat = math.NaN()
		}

		if data[chunks[0]] == nil {
			data[chunks[0]] = &PlotResult{Info: make(map[string]types.PlotValue)}
		}

		data[chunks[0]].Info[chunks[1]] = types.PlotValue(chunkFloat)
	}
}

func rrdSetGraph(graph *rrd.Grapher, serieName, itemName string, percentiles []float64) {
	graph.VDef(serieName+"-min", serieName+",MINIMUM")
	graph.Print(serieName+"-min", itemName+",min,%lf")

	graph.VDef(serieName+"-avg", serieName+",AVERAGE")
	graph.Print(serieName+"-avg", itemName+",avg,%lf")

	graph.VDef(serieName+"-max", serieName+",MAXIMUM")
	graph.Print(serieName+"-max", itemName+",max,%lf")

	graph.VDef(serieName+"-last", serieName+",LAST")
	graph.Print(serieName+"-last", itemName+",last,%lf")

	for index, percentile := range percentiles {
		graph.CDef(fmt.Sprintf("%s-cdef%d", serieName, index),
			fmt.Sprintf("%s,UN,0,%s,IF", serieName, serieName))
		graph.VDef(fmt.Sprintf("%s-vdef%d", serieName, index),
			fmt.Sprintf("%s-cdef%d,%f,PERCENT", serieName, index, percentile))

		if percentile-float64(int(percentile)) != 0 {
			graph.Print(fmt.Sprintf("%s-vdef%d", serieName, index),
				fmt.Sprintf("%s,%.2fth,%%lf", itemName, percentile))
		} else {
			graph.Print(fmt.Sprintf("%s-vdef%d", serieName, index),
				fmt.Sprintf("%s,%.0fth,%%lf", itemName, percentile))
		}
	}
}

func init() {
	ConnectorHandlers["rrd"] = func(origin *Origin, config map[string]string) error {
		if _, ok := config["path"]; !ok {
			return fmt.Errorf("missing `path' mandatory connector setting")
		} else if _, ok := config["pattern"]; !ok {
			return fmt.Errorf("missing `pattern' mandatory connector setting")
		}

		origin.Connector = &RRDConnectorHandler{
			Path:    config["path"],
			Pattern: config["pattern"],
			origin:  origin,
			metrics: make(map[string]map[string]*RRDMetric),
		}

		return nil
	}
}