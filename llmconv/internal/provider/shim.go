package provider

// AntigravityBase stands in for upstream's provider.AntigravityBase, which
// maps one of Antigravity's per-effort model ids to the model magpie offers
// for it by reading the live model catalog of a signed-in Antigravity
// account. This package has no accounts and no catalog, and plain Gemini
// requests never ask (the Gemini builder calls the Code Assist builder as
// agent "gemini"), so it knows no such id.
func AntigravityBase(id string) (base, effort string, ok bool) {
	return "", "", false
}

// AntigravitySentID stands in for upstream's provider.AntigravitySentID,
// which picks the per-effort variant of a model from the same live catalog.
// With no catalog there are no variants, and a model goes out under its own
// id. Only the Code Assist envelope builder asks, for an Antigravity
// account, which this package's API never builds for.
func AntigravitySentID(model, effort string) string {
	return model
}
