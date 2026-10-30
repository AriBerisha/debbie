package ui

import (
	"errors"
	"fmt"
	"time"

	"debbie/internal/provider"
)

// retryText says why a request is tried again, and when (SPEC §4.5).
func retryText(r provider.Retrying) string {
	why := "The connection dropped"
	var apiErr *provider.APIError
	if errors.As(r.Err, &apiErr) {
		switch {
		case apiErr.Status == 529 || apiErr.Type == "overloaded_error":
			why = apiErr.Provider + " is overloaded"
		case apiErr.Status == 429:
			why = apiErr.Provider + " is limiting requests"
		case apiErr.Status != 0:
			why = fmt.Sprintf("%s failed (HTTP %d)", apiErr.Provider, apiErr.Status)
		default:
			why = apiErr.Provider + " reported an error"
		}
	}
	wait := max(time.Second, r.Wait.Round(time.Second))
	return fmt.Sprintf("%s; trying again in %s (%d of %d)", why, wait, r.Attempt, r.Of)
}

// refusalText says that the model declined, and why when it said: a
// safety classifier's category, such as cyber.
func refusalText(detail string) string {
	s := "The model declined this request"
	if detail != "" {
		s += " (" + detail + ")"
	}
	return s + ". Nothing it started was run. Rephrase the request, or switch models with /provider use."
}
