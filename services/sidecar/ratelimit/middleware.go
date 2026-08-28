package ratelimit

import (
	"net/http"
	"strconv"
)

func Middleware(l *Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.Allow() {
			next.ServeHTTP(w, r)
			return
		}

		retrySeconds := int(l.RetryAfter().Seconds() + 1)
		w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	})
}
