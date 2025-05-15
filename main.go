package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	mongoClient   *mongo.Client
	mongoDatabase *mongo.Database
	mongoLogsColl *mongo.Collection
)

var dangerousChars = regexp.MustCompile(`[;]`)

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

func graphqlMiddleware(target *url.URL) http.HandlerFunc {
	proxy := httputil.NewSingleHostReverseProxy(target)

	return func(w http.ResponseWriter, r *http.Request) {
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

		go logToMongo(ctx, r.RemoteAddr, originalQuery, cleanedQuery)

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

	// Optional: Ping the database
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

	target, _ := url.Parse("http://localhost:9090")

	http.HandleFunc("/public", graphqlMiddleware(target))

	log.Println("Go middleware proxy listening on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
