package server

// Route path fragments. Centralised so a rename is one edit and reviewers can
// see the full URL vocabulary at a glance. Used by handlers that emit links
// back to the marketing site or the API itself.

const (
	// Marketing site paths (rendered by the static website, not this binary).
	// Concatenated with server.Config.MarketingURL at call sites.
	pathMarketingHome           = "/"
	pathMarketingDashboard      = "/dashboard"
	pathMarketingDashboardPage  = "/dashboard.html"
	pathMarketingPricing        = "/pricing"
	pathMarketingPricingPage    = "/pricing.html"
	pathMarketingStart          = "/start"
	pathMarketingStartErrorQS   = "/start?error="

	// API paths served by this binary. Concatenated with server.baseURL when
	// surfaced back to clients (e.g. dashboard JSON, emitted webhook URLs).
	pathAPIBillingSubscription = "/billing/create-subscription"
	pathAPIWebhookReceive      = "/webhook/receive/"
)
