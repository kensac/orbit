package spec

import (
	"fmt"
	"sort"
	"time"
)

// ExpiryLayout is the format of an expires: date.
const ExpiryLayout = "2006-01-02"

// Expiries returns each recorded credential's expiry, midnight UTC of its date. Entries that do not parse are left out; validation has refused them already.
func (d Deployment) Expiries() map[string]time.Time {
	out := make(map[string]time.Time, len(d.Expires))
	for name, v := range d.Expires {
		if t, err := time.Parse(ExpiryLayout, v); err == nil {
			out[name] = t
		}
	}
	return out
}

func (c *Config) validateExpires() []error {
	var errs []error
	names := make([]string, 0, len(c.Deployment.Expires))
	for n := range c.Deployment.Expires {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !credentialNameRE.MatchString(n) {
			errs = append(errs, fmt.Errorf("expires %q: the name is an environment variable (%s)", n, credentialNameRE))
		}
		if _, err := time.Parse(ExpiryLayout, c.Deployment.Expires[n]); err != nil {
			errs = append(errs, fmt.Errorf("expires %s: %q is not a date (YYYY-MM-DD)", n, c.Deployment.Expires[n]))
		}
	}
	return errs
}
