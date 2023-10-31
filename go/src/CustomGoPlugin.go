package main

import (
	"bytes"
	"encoding/json"
	"github.com/TykTechnologies/opentelemetry/trace"
	"github.com/TykTechnologies/tyk/ctx"
	"github.com/TykTechnologies/tyk/log"
	"io"
	"net/http"
	"time"
)

const (
	reportInterval = 30 * time.Second // how often to report agg usages
)

type Resource struct {
	OrderId            string
	ProductId          string
	ConsumerId         string
	ResourceInstanceId string
	Period             map[string]string
	Products           []map[string]interface{}
}

var usageReports chan *Resource

var logger = log.Get()

// AddFooBarHeader adds custom "Foo: Bar" header to the request
func AddFooBarHeader(rw http.ResponseWriter, r *http.Request) {
	apidef := ctx.GetDefinition(r)
	// We create a new span using the context from the incoming request.
	_, newSpan := trace.NewSpanFromContext(r.Context(), "", "GoPlugin_first-span")

	// Ensure that the span is properly ended when the function completes.
	defer newSpan.End()

	// Set a new name for the span.
	newSpan.SetName("AddFooBarHeader Function")

	// Set the status of the span.
	newSpan.SetStatus(trace.SPAN_STATUS_OK, "")

	r.Header.Add("Foo", "Bar2")

	logger.Info("AddFooBarHeader called for API: ", apidef.Name)

	usageReports <- &Resource{
		OrderId:            "orderId",
		ProductId:          "productId",
		ConsumerId:         "consumerId",
		ResourceInstanceId: "resourceInstanceId",
		Period: map[string]string{
			"start": time.Now().UTC().Format(time.RFC3339),
			"end":   time.Now().UTC().Format(time.RFC3339),
		},
		Products: []map[string]interface{}{
			{
				"type":            "FIVE_G",
				"reportedValue":   1,
				"ownerId":         "112c7aa6-eccd-4eff-a06c-f1de2ee79225",
				"measurementType": "REQUEST",
			},
		},
	}
}

func main() {}

func init() {
	logger.Info("--- ClearX aggregator middleware init success! ---- ")
	usageReports = make(chan *Resource)
	go reportsAggregator(reportInterval)
}

func reportsAggregator(updateInterval time.Duration) {
	aggregateUsages := make([]*Resource, 0)
	ticker := time.NewTicker(updateInterval)
	for {
		select {
		case <-ticker.C:
			logger.Info("Reporting usage, aggregate length - ", len(aggregateUsages))
			// report usage and clear usageReports
			for _, r := range aggregateUsages {
				logger.Info("Reporting usage ", r)
				reportUsageToCAL(r)
			}
			aggregateUsages = make([]*Resource, 0)
		case r := <-usageReports:
			logger.Info("Received usage report ", r)
			aggregateUsages = append(aggregateUsages, r)
			// do something with new usage (maybe agg by order ?)
		}
	}
}

func reportUsageToCAL(r *Resource) {
	jsonData, err := json.Marshal(r)

	if err != nil {
		logger.Error(err)
		return
	}
	resp, err := http.Post("https://httpbin.org/post", "application/json", bytes.NewBuffer(jsonData))

	if err != nil {
		logger.Error(err)
		return
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logger.Error(err)
		}
	}(resp.Body)

	var res map[string]interface{}

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		logger.Error(err)
		return
	}

	logger.Info("res - ", res["json"])
}
