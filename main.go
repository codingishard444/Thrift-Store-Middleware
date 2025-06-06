package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	mongoClient   *mongo.Client
	mongoDatabase *mongo.Database
	mongoLogsColl *mongo.Collection
)

var dangerousChars = regexp.MustCompile(`[;&*+#=<>-]`)

var rateLimitStore = make(map[string][]time.Time)

const (
	maxRequestsPerMinute = 50
	rateLimitWindow      = time.Minute
)

func sanitizeGraphQLQuery(query string) string {
	return dangerousChars.ReplaceAllString(query, "")
}

func logToMongo(ctx context.Context, ip, raw, sanitized string) {
	entry := map[string]interface{}{
		"ip":             ip,
		"originalQuery":  raw,
		"sanitizedQuery": sanitized,
		"timestamp":      time.Now(),
	}

	_, err := mongoLogsColl.InsertOne(ctx, entry)
	if err != nil {
		log.Printf("Error writing to MongoDB: %v", err)
	}
}

func extractClientIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		log.Printf("Failed to split remote address: %v", err)
		return remoteAddr
	}
	if ip == "::1" { // IPv6 Loopback Address
		return "127.0.0.1"
	}
	return ip
}

func isRateLimited(ip string) bool {
	now := time.Now()
	requests := rateLimitStore[ip]

	// Remove timestamps outside the current window
	var recentRequests []time.Time
	for _, t := range requests {
		if now.Sub(t) <= rateLimitWindow {
			recentRequests = append(recentRequests, t)
		}
	}

	// Update the map with only recent requests
	rateLimitStore[ip] = recentRequests

	// Check if the IP exceeded the limit
	if len(recentRequests) >= maxRequestsPerMinute {
		return true
	}

	// Add this request timestamp
	rateLimitStore[ip] = append(rateLimitStore[ip], now)
	return false
}

func graphqlMiddleware(target *url.URL) http.HandlerFunc {
	proxy := httputil.NewSingleHostReverseProxy(target)

	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("Access-Control-Allow-Origin")
		resp.Header.Del("Access-Control-Allow-Methods")
		resp.Header.Del("Access-Control-Allow-Headers")
		resp.Header.Del("Access-Control-Allow-Credentials")
		return nil
	}

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:3000")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != http.MethodPost || r.URL.Path != "/public" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		ctx := r.Context()
		var bodyBytes []byte
		if r.Body != nil {
			bodyBytes, _ = io.ReadAll(r.Body)
		}
		var payload map[string]interface{}
		json.Unmarshal(bodyBytes, &payload)

		originalQuery := ""
		cleanedQuery := ""

		if query, ok := payload["query"].(string); ok {
			originalQuery = query
			cleanedQuery = sanitizeGraphQLQuery(query)
			payload["query"] = cleanedQuery
		}

		newBody, _ := json.Marshal(payload)
		r.Body = io.NopCloser(bytes.NewBuffer(newBody))
		r.ContentLength = int64(len(newBody))
		ip := extractClientIP(r.RemoteAddr)
		if isRateLimited(ip) {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		go logToMongo(ctx, ip, originalQuery, cleanedQuery)

		proxy.ServeHTTP(w, r)
	}
}

func initMongo() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}

	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		log.Fatal("MONGO_URI not found in environment")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("MongoDB connection error: %v", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("MongoDB ping failed: %v", err)
	}

	mongoClient = client
	mongoDatabase = client.Database("Middleware_Logs")
	mongoLogsColl = mongoDatabase.Collection("graphql_logs")

	log.Println("Connected to MongoDB")
}

func main() {
	initMongo()

	// target, _ := url.Parse("http://localhost:9090")
	targetStr := os.Getenv("BACKEND_URL")
	target, err := url.Parse(targetStr)
	if err != nil {
		log.Fatalf("Invalid BACKEND_URL: %v", err)
	}
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/public", graphqlMiddleware(target))

	log.Println("Go middleware proxy listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
