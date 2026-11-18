package purelymail

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

type RoutingRule struct {
	ID              int64    `json:"id"`
	DomainName      string   `json:"domainName"`
	Prefix          bool     `json:"prefix"`
	MatchUser       string   `json:"matchUser"`
	TargetAddresses []string `json:"targetAddresses"`
	Catchall        bool     `json:"catchall"`
}

func (c *Client) ListRoutingRules(ctx context.Context) ([]RoutingRule, error) {
	var result struct {
		Rules []RoutingRule `json:"rules"`
	}
	if err := c.post(ctx, "listRoutingRules", struct{}{}, &result); err != nil {
		return nil, err
	}
	return result.Rules, nil
}

func (c *Client) DeleteRoutingRule(ctx context.Context, id int64) error {
	return c.post(ctx, "deleteRoutingRule", struct {
		RoutingRuleID int64 `json:"routingRuleId"`
	}{id}, nil)
}

type CreateRoutingRuleRequest struct {
	DomainName      string   `json:"domainName"`
	Prefix          bool     `json:"prefix"`
	MatchUser       string   `json:"matchUser"`
	TargetAddresses []string `json:"targetAddresses"`
	Catchall        bool     `json:"catchall"`
}

func (c *Client) CreateRoutingRuleVerified(ctx context.Context, request CreateRoutingRuleRequest) (*RoutingRule, error) {
	if len(request.TargetAddresses) == 0 {
		return nil, errors.New("a routing rule needs at least one target address")
	}
	if slices.Contains(request.TargetAddresses, "") {
		return nil, errors.New("a routing rule target address cannot be empty")
	}
	if err := c.post(ctx, "createRoutingRule", request, nil); err != nil {
		return nil, err
	}
	rules, err := c.ListRoutingRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("the create may have worked but the verify read failed: %w", err)
	}
	// prefix stays out of the match: its effect on collisions is untested
	for i := range rules {
		rule := &rules[i]
		if rule.DomainName == request.DomainName && rule.MatchUser == request.MatchUser &&
			rule.Catchall == request.Catchall && slices.Equal(rule.TargetAddresses, request.TargetAddresses) {
			return rule, nil
		}
	}
	return nil, ErrSilentNoOp
}
