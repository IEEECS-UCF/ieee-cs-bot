package mail

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

func validLocal(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func validEmail(address string) bool {
	local, domain, found := strings.Cut(address, "@")
	return found && local != "" && strings.Contains(domain, ".") && !strings.Contains(domain, "@")
}

func (c *Cog) cleanName(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if local, domain, found := strings.Cut(v, "@"); found {
		if domain != c.domain {
			return "", fmt.Errorf("`%s` is not on %s", v, c.domain)
		}
		v = local
	}
	if !validLocal(v) {
		return "", errors.New("a name can use a-z, digits, `.`, `-` and `_`")
	}
	return v, nil
}

func (c *Cog) cleanGroup(value string) (string, error) {
	if strings.TrimSpace(value) == "*" {
		return "*", nil
	}
	return c.cleanName(value)
}

// a subdomain of the managed domain is allowed, the domain itself is not.
func (c *Cog) cleanResetEmail(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if !validEmail(v) {
		return "", fmt.Errorf("`%s` doesn't look like an email address", v)
	}
	if strings.HasSuffix(v, "@"+c.domain) {
		return "", fmt.Errorf("the recovery email cannot be on %s itself", c.domain)
	}
	return v, nil
}

// a bare local part gets @domain.
func (c *Cog) cleanMember(value string) (address string, external bool, err error) {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return "", false, errors.New("the member is empty")
	}
	if !strings.Contains(v, "@") {
		if !validLocal(v) {
			return "", false, errors.New("a member name can use a-z, digits, `.`, `-` and `_`")
		}
		return v + "@" + c.domain, false, nil
	}
	if !validEmail(v) {
		return "", false, fmt.Errorf("`%s` doesn't look like an email address", v)
	}
	return v, !strings.HasSuffix(v, "@"+c.domain), nil
}

// userNameFrom builds jsmith from Jane Smith. "" means characters outside
// a-z remain, and the operator must supply username.
func userNameFrom(first, last string) string {
	clean := func(s string) string {
		s = strings.NewReplacer(" ", "", "-", "", "'", "").Replace(strings.ToLower(strings.TrimSpace(s)))
		for _, r := range s {
			if r < 'a' || r > 'z' {
				return ""
			}
		}
		return s
	}
	f, l := clean(first), clean(last)
	if f == "" || l == "" {
		return ""
	}
	return f[:1] + l
}

const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// nobody sees it: the caller sends it to the api and drops it.
func randomPassword() (string, error) {
	out := make([]byte, 32)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}
