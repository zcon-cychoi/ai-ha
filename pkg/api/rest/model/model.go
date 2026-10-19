// Package model defines the request and response types exchanged by the AI-HA
// REST API.
package model

// SimpleMsg is the generic response body used for status and error replies.
type SimpleMsg struct {
	Message string `json:"message" example:"Service ready"`
}
