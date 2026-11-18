package purelymail

import "context"

type CreateUserRequest struct {
	UserName                 string `json:"userName"`
	DomainName               string `json:"domainName"`
	Password                 string `json:"password"`
	EnablePasswordReset      bool   `json:"enablePasswordReset"`
	RecoveryEmail            string `json:"recoveryEmail"`
	RecoveryEmailDescription string `json:"recoveryEmailDescription"`
}

func (c *Client) CreateUser(ctx context.Context, request CreateUserRequest) error {
	return c.post(ctx, "createUser", request, nil)
}

func (c *Client) DeleteUser(ctx context.Context, address string) error {
	return c.post(ctx, "deleteUser", userName{address}, nil)
}

type User struct {
	RecoveryEnabled bool `json:"recoveryEnabled"`
}

func (c *Client) GetUser(ctx context.Context, address string) (*User, error) {
	var user User
	if err := c.post(ctx, "getUser", userName{address}, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// ModifyUserRequest always carries both fields: enablePasswordReset alone
// returns success and changes nothing.
type ModifyUserRequest struct {
	UserName            string `json:"userName"`
	NewPassword         string `json:"newPassword"`
	EnablePasswordReset bool   `json:"enablePasswordReset"`
}

func (c *Client) ModifyUser(ctx context.Context, request ModifyUserRequest) error {
	return c.post(ctx, "modifyUser", request, nil)
}

// api's documented cap of 1000 results
func (c *Client) ListUsers(ctx context.Context) (addresses []string, more bool, err error) {
	var result struct {
		Users []string `json:"users"`
	}
	if err := c.post(ctx, "listUser", struct{}{}, &result); err != nil {
		return nil, false, err
	}
	return result.Users, len(result.Users) >= 1000, nil
}

type userName struct {
	UserName string `json:"userName"`
}
