package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chi_middleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-redis/redis/v8"
	"github.com/hashicorp/consul/api"
	"github.com/sahasourav17/goGateway.git/internal/config"
	"github.com/sahasourav17/goGateway.git/internal/middleware"
)

var (
	// CurrentRouter is the currently active router that serves traffic
	CurrentRouter http.Handler
	// RouterMutex protects CurrentRouter during hot reloading
	RouterMutex sync.RWMutex
)

const consulKey = "gateway/config"

func UpdateRouter(consulClient *api.Client, redisClient *redis.Client) {
	log.Println("Updating router configuration from Consul...")
	kvPair, _, err := consulClient.KV().Get(consulKey, nil)
	if err != nil || kvPair == nil {
		log.Printf("Could not fetch or find config in Consul ('%s'): %v", consulKey, err)
		return
	}

	var cfg config.Config
	if err := json.Unmarshal(kvPair.Value, &cfg); err != nil {
		log.Printf("Error parsing config from consul: %v", err)
		return
	}

	r := chi.NewRouter()
	r.Use(chi_middleware.RequestID)
	r.Use(middleware.NewStructuredLogger(middleware.InitLogger()))
	r.Use(chi_middleware.Recoverer)

	for _, route := range cfg.Routes {
		service, ok := cfg.Services[route.ServiceName]
		if !ok {
			log.Printf("Service '%s' for route '%s' not found, skipping.", route.ServiceName, route.PathPrefix)
			continue
		}

		targetURL, err := url.Parse(service.URL)
		if err != nil {
			log.Printf("Invalid URL for service '%s': %v", service.Name, err)
			continue
		}

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		path := route.PathPrefix

		var handler http.Handler = http.StripPrefix(path, proxy)

		// circuit breaker
		handler = middleware.CircuitBreaker(handler, route.ServiceName)

		// rate limit
		if route.Middleware.RateLimit != nil {
			log.Printf("Applying rate limit to %s", path)
			handler = middleware.RateLimiter(redisClient, route.Middleware.RateLimit)(handler)
		}

		// auth check
		if route.AuthRequired {
			handler = middleware.AuthMiddleware(handler)
		}

		r.Handle(path+"/*", handler)
		log.Printf("Setting up route: %s -> %s (Auth: %v)", path, service.URL, route.AuthRequired)
	}

	// Replace the current router with the new one
	RouterMutex.Lock()
	CurrentRouter = r
	RouterMutex.Unlock()

	log.Println("Router configuration updated successfully.")
}

func WatchConsul(consulClient *api.Client, redisClient *redis.Client) {
	var lastIndex uint64
	for {
		opts := &api.QueryOptions{
			WaitIndex: lastIndex,
		}
		kvPair, meta, err := consulClient.KV().Get(consulKey, opts)
		if err != nil {
			log.Printf("Error watching consul: %v", err)
			time.Sleep(5 * time.Second) // Wait before retrying
			continue
		}

		// if index is different, it means the config has changed
		if meta.LastIndex != lastIndex {
			if kvPair != nil {
				UpdateRouter(consulClient, redisClient)
				lastIndex = meta.LastIndex
			}
		}
	}
}
