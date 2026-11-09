package fetch

import (
	"context"
	"encoding/json"
	"errors"
)

// errNotJSON: a 200 answer that isn't JSON, like a captive portal's or a
// CDN's HTML page.
var errNotJSON = errors.New("not JSON")

// ValidJSON is a Validate for JSON APIs: an answer that isn't JSON is
// neither cached nor used.
func ValidJSON(data []byte) error {
	if !json.Valid(data) {
		return errNotJSON
	}
	return nil
}

// GetJSON is Get for a JSON API: the answer must be JSON (ValidJSON, before
// r's own Validate) and is decoded into out.
func (c *Client) GetJSON(ctx context.Context, r Request, out any) error {
	if own := r.Validate; own != nil {
		r.Validate = func(data []byte) error {
			if err := ValidJSON(data); err != nil {
				return err
			}
			return own(data)
		}
	} else {
		r.Validate = ValidJSON
	}
	data, err := c.Get(ctx, r)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
