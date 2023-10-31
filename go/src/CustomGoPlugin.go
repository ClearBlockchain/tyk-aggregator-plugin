package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/TykTechnologies/opentelemetry/trace"
	"github.com/TykTechnologies/tyk/apidef"
	"github.com/TykTechnologies/tyk/ctx"
	"github.com/TykTechnologies/tyk/log"
	"github.com/go-redis/redis/v8"
	"golang.org/x/oauth2/clientcredentials"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

const (
	reportInterval = 30 * time.Second // how often to report agg usages
)

type UsageReport struct {
	OrderId            string
	ProductId          string
	ConsumerId         string
	ResourceInstanceId string
	Period             map[string]string
	Products           []map[string]interface{}
}

var usageReports chan *UsageReport
var aggregateUsages []*UsageReport
var logger = log.Get()

var redisClient = redis.NewClient(&redis.Options{
	Addr:     "10.10.8.21:30379",
	Password: "DimAJnkE3R", // no password set
	DB:       0,            // use default database
})

func copyHeaders(src, dst *http.Request) {
	for k, vv := range src.Header {
		for _, v := range vv {
			dst.Header.Add(k, v)
		}
	}
}

// AggregatorMiddleware is a middleware that will be called for every request
func AggregatorMiddleware(rw http.ResponseWriter, r *http.Request) {
	apidef := ctx.GetDefinition(r)
	// We create a new span using the context from the incoming request.
	_, newSpan := trace.NewSpanFromContext(r.Context(), "", "GoPlugin_first-span")

	// Ensure that the span is properly ended when the function completes.
	defer newSpan.End()

	// Set a new name for the span.
	newSpan.SetName("AggregatorMiddleware Function")

	// Set the status of the span.
	newSpan.SetStatus(trace.SPAN_STATUS_OK, "")

	// start logic

	fmt.Println("inside AggregatorMiddleware")
	apiDef := ctx.GetDefinition(r)

	forwardToIdentity, forwardToInfo, url, authType, err := getForwardInfo(r, apiDef)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write([]byte("Error:  " + err.Error()))
		return
	}

	fmt.Println("URL:", url+r.URL.Path, authType)
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logger.Error(err)
		}
	}(r.Body)

	var res map[string]interface{}

	err = json.NewDecoder(r.Body).Decode(&res)
	if err != nil {
		logger.Error(err)
		return
	}

	logger.Info("res body - ", res)

	var responseBody string
	var responseStatusCode int
	var client *http.Client
	req, err := http.NewRequest(r.Method, url, bytes.NewBuffer([]byte(res["body"].(string))))
	if err != nil {
		fmt.Println("Error creating request:", err)
		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write([]byte("Error: creating request"))
		return
	}
	copyHeaders(r, req)

	client, err = configureAuthClient(r, authType, forwardToIdentity, forwardToInfo, client, req)
	if err != nil {
		fmt.Println("Error configuring client:", err)
		rw.WriteHeader(http.StatusUnauthorized)
		rw.Write([]byte("Error " + err.Error()))
		return
	}

	// Send the request
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("PROXY request error:", err)

		rw.WriteHeader(http.StatusInternalServerError)
		rw.Write([]byte("PROXY request error"))

		return
	}
	defer resp.Body.Close()

	// Saving GET result and extracting data
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		responseBody = "Error reading base response body: " + err.Error()
		responseStatusCode = http.StatusInternalServerError
	} else {
		responseBody = string(body)
		responseStatusCode = resp.StatusCode
	}
	fmt.Println("Response body:", string(body)) // Print the entire response body

	// return response to user asap
	rw.WriteHeader(responseStatusCode)
	rw.Write([]byte(responseBody))
	// return without report usage if got an error
	if responseStatusCode == http.StatusInternalServerError {
		return
	}

	logger.Info("AggregatorMiddleware called for API: ", apidef.Name)

	usageReports <- &UsageReport{
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

func configureAuthClient(r *http.Request, authType string, forwardToIdentity string, forwardToInfo map[string]interface{}, client *http.Client, req *http.Request) (*http.Client, error) {
	if authType == "oidc" {
		// here check if I already have a token save in redis to talk to operator and if not get token now
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			fmt.Println("Error: Authorization header is missing so no token.")
			return nil, errors.New("authorization header is missing")

		}
		reqTokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		if reqTokenStr == "" {
			fmt.Println("Error: Req Bearer token is missing")
			return nil, errors.New("req bearer token is missing")
		}

		userId, aggClientId, err := getSubFromToken(reqTokenStr)
		if err != nil {
			fmt.Printf("Error getting userId from token: %v\n", err)
			return nil, errors.New("could not get sub claim from user token")
		}
		alreadyExistsToken, err := getTokenFromRedis(userId, aggClientId, forwardToIdentity)
		if err != nil {
			fmt.Println("Error getting token from Redis so asking for new token. error:", err)
			clientID := forwardToInfo["clientId"].(string)
			clientSecret := forwardToInfo["clientSecret"].(string)
			tokenURL := forwardToInfo["getTokenUrl"].(string)

			// Set up the config for the client credentials flow
			conf := &clientcredentials.Config{
				ClientID:     clientID,
				ClientSecret: clientSecret,
				TokenURL:     tokenURL,
				Scopes:       []string{}, // specify required scopes if any
			}

			// http.Client will automatically handle token refreshing
			client = conf.Client(context.Background())
			fmt.Println("my client")
			fmt.Println(client)
		} else {
			// Add the "Authorization" header with the "Bearer" token
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", alreadyExistsToken))
			// Create a new HTTP client
			client = &http.Client{}
			// should I add the clienID also ?
		}

	} else if authType == "base" {
		// Bearer token for authorization
		token := forwardToInfo["token"].(string)

		// Create a new GET request

		// Add the "Authorization" header with the "basic" token
		req.Header.Set("Authorization", fmt.Sprintf("Basic %s", token))
		// Create a new HTTP client
		client = &http.Client{}

		//TOOD: send response back to user
	} else if authType == "keyless" {
		client = &http.Client{}

	} else {
		// Handle case where authType is not recognized
		return nil, errors.New("unknown authType")
	}
	return client, nil
}

func getForwardInfo(r *http.Request, apiDef *apidef.APIDefinition) (string, map[string]interface{}, string, string, error) {
	forwardOptions, ok := apiDef.ConfigData["forwardOptions"].(map[string]interface{})
	if !ok {
		fmt.Println("Error: forwardOptions is not a map")
		return "", nil, "", "", errors.New("forwardOptions is not a map")
	}

	// Extract the value of the "network" header
	forwardToIdentity := r.Header.Get("network")
	if forwardToIdentity == "" {
		// Handle case where header is missing or value is empty
		fmt.Println("Error: Missing header: network")
		return "", nil, "", "", errors.New("missing header: network")
	}

	forwardToInfoRes, exists := forwardOptions[forwardToIdentity]
	if !exists {
		fmt.Println("Error: Key does not exist")
		return "", nil, "", "", errors.New("key does not exist")
	}

	forwardToInfo, ok := forwardToInfoRes.(map[string]interface{})
	if !ok {
		fmt.Println("Error: option is not a map")
		return "", nil, "", "", errors.New("option is not a map")
	}

	url, exists := forwardToInfo["url"].(string)
	if !exists {
		fmt.Println("Error: url does not exist")
		return "", nil, "", "", errors.New("url does not exist")
	}
	authType, exists := forwardToInfo["authType"].(string)
	if !exists {
		fmt.Println("Error: authType does not exist")
		return "", nil, "", "", errors.New("authType does not exist")
	}
	return forwardToIdentity, forwardToInfo, url, authType, nil
}

func getTokenFromRedis(userId string, myClientId string, forwardToIdentity string) (string, error) {
	redisKey := userId + ":" + myClientId + ":" + forwardToIdentity
	ctx := context.Background()
	redisToken, err := redisClient.Get(ctx, redisKey).Result()
	if err != nil {
		return "", fmt.Errorf("cannot find redisData so guessing no token is available for key %s: %w", redisKey, err)
	}
	return redisToken, nil
}

func getSubFromToken(tokenStr string) (string, string, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return "", "", errors.New("invalid token format")
	}

	// Base64 decode the payload
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("error decoding token payload: %w", err)
	}

	// Unmarshal the payload into a map
	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", fmt.Errorf("error unmarshaling token payload: %w", err)
	}
	fmt.Println("payload", claims["client_id"].(string))

	// Extract the 'sub' field from the map
	sub, ok := claims["sub"].(string)
	if !ok {
		return "", "", errors.New("sub claim is missing in token")
	}

	aggClientId, ok := claims["client_id"].(string)
	if !ok {
		return "", "", errors.New("client_id claim is missing in token")
	}

	return sub, aggClientId, nil
}

func main() {}

func init() {
	logger.Info("--- ClearX aggregator middleware init success! ---- ")
	usageReports = make(chan *UsageReport)
	aggregateUsages = make([]*UsageReport, 0)
	go reportsAggregator(reportInterval)

	reloadUnReportedUsage()
}

func reloadUnReportedUsage() {
	ctx2 := context.Background()
	iter := redisClient.Scan(ctx2, 0, "usageReport:*", 0).Iterator()
	for iter.Next(ctx2) {
		fmt.Println("keys", iter.Val())
		val, err := redisClient.Get(ctx2, iter.Val()).Result()
		if err != nil {
			logger.Error("Error getting key from redis ", err)
			continue
		}
		fmt.Println("val", val)
		var usage UsageReport
		err = json.Unmarshal([]byte(val), &usage)
		if err != nil {
			logger.Error("Error unmarshalling json ", err)
			continue
		}
		fmt.Println("usage", usage)
		aggregateUsages = append(aggregateUsages, &usage)
	}
	if err := iter.Err(); err != nil {
		panic(err)
	}
}

func reportsAggregator(updateInterval time.Duration) {
	ticker := time.NewTicker(updateInterval)
	for {
		select {
		case <-ticker.C:
			logger.Info("Reporting usage, aggregate length - ", len(aggregateUsages))
			// report usage and clear usageReports
			for i := len(aggregateUsages) - 1; i >= 0; i-- {
				r := aggregateUsages[i]
				logger.Info("Reporting usage ", r)
				err := reportUsageToCAL(r)
				if err != nil {
					logger.Error("Error reporting usage ", err)
					continue
				}
				// Swap the current element with the last element
				aggregateUsages[i] = aggregateUsages[len(aggregateUsages)-1]
				// Reduce the slice by one, effectively removing the last element
				aggregateUsages = aggregateUsages[:len(aggregateUsages)-1]
				// delete key from redis
				usageRedisKey := "usageReport:" + r.OrderId + r.Period["start"]
				redisClient.Del(context.Background(), usageRedisKey)
			}
		case r := <-usageReports:
			logger.Info("Received usage report ", r)
			aggregateUsages = append(aggregateUsages, r)
			bytes, _ := json.Marshal(r)
			usageStr := string(bytes)
			usageRedisKey := "usageReport:" + r.OrderId + r.Period["start"]
			redisClient.Set(context.Background(), usageRedisKey, usageStr, 0)

			// do something with new usage (maybe agg by order ?)
		}
	}
}

func reportUsageToCAL(r *UsageReport) error {
	jsonData, err := json.Marshal(r)

	if err != nil {
		logger.Error("Error marshalling json data" + err.Error())
		return err
	}
	resp, err := http.Post("https://httpbin.org/post", "application/json", bytes.NewBuffer(jsonData))

	if err != nil {
		logger.Error("Error posting json data" + err.Error())
		return err
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			logger.Error("Error closing body" + err.Error())
		}
	}(resp.Body)

	var res map[string]interface{}

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		logger.Error("Error decoding json data" + err.Error())
		return err
	}

	logger.Info("res - ", res["json"])
	return nil
}
