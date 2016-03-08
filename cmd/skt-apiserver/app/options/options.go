// Package options contains flags and options for initializing an apiserver
package options

import ()

// APIServer runs an api server.
type APIServer struct {
}

// NewAPIServer creates a new APIServer object with default parameters
func NewAPIServer() *APIServer {
	s := APIServer{}
	return &s
}
