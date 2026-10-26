package ingest

// Response is the backend's reply, from the ingest route: how many rows were accepted vs. rejected, and why.
// This is the customer's only visibility into a partial failure, so the agent logs every
// rejection/error clearly rather than only checking the HTTP status.
type Response struct {
	Accepted int      `json:"accepted"`
	Rejected int      `json:"rejected"`
	Errors   []string `json:"errors,omitempty"`

	// The fields below are returned only by the AI-gateway endpoints (litellm-cost,
	// bifrost-cost) and are absent from the Kubernetes endpoint's reply. All are optional.

	// UsageAccepted counts token-telemetry rows written.
	UsageAccepted int `json:"usage_accepted,omitempty"`
	// HeldProviders names upstream providers whose spend was NOT written because nobody has yet
	// decided how to count them (or a direct connector already covers them). That is the
	// double-count guard working, not an error, but the operator needs to know.
	HeldProviders []string `json:"held_providers,omitempty"`
	// Activity is the backend's report on the optional per-row activity label. Counts only.
	Activity *ActivityReport `json:"activity,omitempty"`
}

// ActivityReport mirrors the `activity` block of the litellm-cost response.
type ActivityReport struct {
	Distinct       int `json:"distinct"`
	Cap            int `json:"cap"`
	InvalidRows    int `json:"invalid_rows"`
	OverflowRows   int `json:"overflow_rows"`
	OverflowValues int `json:"overflow_values"`
}
