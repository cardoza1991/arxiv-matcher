package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Paper represents a processed arXiv paper.
type Paper struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Abstract string   `json:"abstract"`
	Authors  []string `json:"authors"`
	Category string   `json:"category"`
	Date     string   `json:"date"`
}

// Cache structure with expiration.
type Cache struct {
	sync.RWMutex
	items map[string]cacheItem
}

type cacheItem struct {
	data       []Paper
	expiration time.Time
}

var (
	// Global cache.
	cache = &Cache{
		items: make(map[string]cacheItem),
	}
	// Cache duration can be tuned.
	cacheDuration = 1 * time.Hour
)

// Global HTTP client with connection pooling.
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false, // enable compression if server supports it
	},
}

func main() {
	// Start background cache cleanup.
	go startCacheCleanup()

	http.HandleFunc("/api/papers", handlePapers)
	http.Handle("/", http.FileServer(http.Dir("static")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// startCacheCleanup periodically purges expired cache entries.
func startCacheCleanup() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		cache.Lock()
		for key, item := range cache.items {
			if now.After(item.expiration) {
				delete(cache.items, key)
			}
		}
		cache.Unlock()
	}
}

func handlePapers(w http.ResponseWriter, r *http.Request) {
	// Enable CORS.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Support multiple query parameters; trim whitespace.
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		query = "machine learning"
	}

	// Use request timeout context.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Check cache first.
	if papers := getFromCache(query); papers != nil {
		sendJSONResponse(w, papers)
		return
	}

	// Fetch from arXiv.
	papers, err := fetchFromArXiv(ctx, query)
	if err != nil {
		log.Printf("Error fetching papers: %v", err)
		http.Error(w, "Failed to fetch papers", http.StatusInternalServerError)
		return
	}

	// Store result in cache.
	cache.Lock()
	cache.items[query] = cacheItem{
		data:       papers,
		expiration: time.Now().Add(cacheDuration),
	}
	cache.Unlock()

	sendJSONResponse(w, papers)
}

func getFromCache(query string) []Paper {
	cache.RLock()
	defer cache.RUnlock()

	if item, exists := cache.items[query]; exists {
		if time.Now().Before(item.expiration) {
			return item.data
		}
	}
	return nil
}

func fetchFromArXiv(ctx context.Context, query string) ([]Paper, error) {
	// Use URL encoding if necessary.
	// Here we assume the query is simple.
	url := fmt.Sprintf("http://export.arxiv.org/api/query?search_query=all:%s&start=0&max_results=20", query)

	// Create request with context.
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	// Use the global httpClient.
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making request: %v", err)
	}
	defer resp.Body.Close()

	var feed struct {
		Entries []struct {
			ID        string `xml:"id"`
			Title     string `xml:"title"`
			Summary   string `xml:"summary"`
			Published string `xml:"published"`
			Authors   []struct {
				Name string `xml:"name"`
			} `xml:"author"`
			Categories []struct {
				Term string `xml:"term,attr"`
			} `xml:"category"`
		} `xml:"entry"`
	}

	if err := xml.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, fmt.Errorf("error decoding XML: %v", err)
	}

	var papers []Paper
	for _, entry := range feed.Entries {
		authors := make([]string, len(entry.Authors))
		for i, author := range entry.Authors {
			authors[i] = author.Name
		}

		category := "unknown"
		if len(entry.Categories) > 0 {
			category = entry.Categories[0].Term
		}

		// Ensure the date string is long enough.
		date := entry.Published
		if len(date) >= 10 {
			date = date[:10]
		}

		papers = append(papers, Paper{
			ID:       entry.ID,
			Title:    entry.Title,
			Abstract: entry.Summary,
			Authors:  authors,
			Category: category,
			Date:     date,
		})
	}

	return papers, nil
}

func sendJSONResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	// Setting the CORS header again in case it was removed.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}
