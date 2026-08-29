package ingest

// Response is the backend's reply, per the design doc's §1 ingest route (mirroring
// POST /api/ingest/usage's response shape): how many rows were accepted vs. rejected, and why.
// This is the customer's only visibility into a partial failure, so the agent logs every
// rejection/error clearly rather than only checking the HTTP status.
type Response struct {
	Accepted int      `json:"accepted"`
	Rejected int      `json:"rejected"`
	Errors   []string `json:"errors,omitempty"`
}
