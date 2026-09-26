package handler

import (
	"errors"
	"net/http"
)

// LimitBody caps every request body at max bytes.
func LimitBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

// bodyError writes 413 if err came from LimitBody, otherwise 400 with msg.
func bodyError(w http.ResponseWriter, err error, msg string) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, msg, http.StatusBadRequest)
}
