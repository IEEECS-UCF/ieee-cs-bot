package purelymail

import "context"

type ResetMethod struct {
	Type        string `json:"type"` // "email" or "phone"
	Target      string `json:"target"`
	Description string `json:"description"` // the reset page shows it to the public
}

func (c *Client) ListPasswordReset(ctx context.Context, address string) ([]ResetMethod, error) {
	// the result field is named users, not methods
	var result struct {
		Methods []ResetMethod `json:"users"`
	}
	if err := c.post(ctx, "listPasswordReset", userName{address}, &result); err != nil {
		return nil, err
	}
	return result.Methods, nil
}

// the api's documented replace (existingTarget) returns "Reset method not
// found" in every tested case, so a replace is an add, then a delete.
type UpsertPasswordResetRequest struct {
	UserName    string `json:"userName"`
	Type        string `json:"type"`
	Target      string `json:"target"`
	Description string `json:"description"`
}

func (c *Client) UpsertPasswordReset(ctx context.Context, request UpsertPasswordResetRequest) error {
	return c.post(ctx, "upsertPasswordReset", request, nil)
}

type DeletePasswordResetRequest struct {
	UserName string `json:"userName"`
	Type     string `json:"type"`
	Target   string `json:"target"`
}

func (c *Client) DeletePasswordReset(ctx context.Context, request DeletePasswordResetRequest) error {
	return c.post(ctx, "deletePasswordReset", request, nil)
}
