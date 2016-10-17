package config

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type User struct {
	canViewNumMedia    bool
	canViewMessages    bool
	canViewMessageFrom bool
	canViewMessageTo   bool
}

type UserSettings struct {
	CanViewNumMedia    bool
	CanViewMessages    bool
	CanViewMessageFrom bool
	CanViewMessageTo   bool
}

// AllUserSettings returns a UserSettings value with the widest possible set of
// permissions.
func AllUserSettings() *UserSettings {
	return &UserSettings{
		CanViewNumMedia:    true,
		CanViewMessages:    true,
		CanViewMessageFrom: true,
		CanViewMessageTo:   true,
	}
}

func NewUser(us *UserSettings) *User {
	if us == nil {
		us = &UserSettings{}
	}
	return &User{
		canViewNumMedia:    us.CanViewNumMedia,
		canViewMessages:    us.CanViewMessages,
		canViewMessageFrom: us.CanViewMessageFrom,
		canViewMessageTo:   us.CanViewMessageTo,
	}
}

func (u *User) CanViewNumMedia() bool {
	return u.CanViewMessages() && u.canViewNumMedia
}

func (u *User) CanViewMessages() bool {
	return u.canViewMessages
}

func (u *User) CanViewMessageFrom() bool {
	return u.CanViewMessages() && u.canViewMessageFrom
}

func (u *User) CanViewMessageTo() bool {
	return u.CanViewMessages() && u.canViewMessageTo
}

type Permission struct {
	maxResourceAge time.Duration
}

var ErrTooOld = errors.New("Cannot access this resource because its age exceeds the viewable limit")
var PermissionDenied = errors.New("You do not have permission to access that information")

func (p *Permission) MaxResourceAge() time.Duration {
	return p.maxResourceAge
}

func NewPermission(maxResourceAge time.Duration) *Permission {
	return &Permission{
		maxResourceAge: maxResourceAge,
	}
}

// TODO store in database or something
var userMap = make(map[string]*User)

// TODO fix
func AddUser(name string, u *User) {
	userMap[name] = u
}

type ctxVar int

var userKey ctxVar = 0

// Auth finds the authenticating User for the request, or returns an error if
// none could be found. Auth also sets the user in the request's context.
func AuthUser(r *http.Request) (*http.Request, *User, error) {
	user, _, ok := r.BasicAuth()
	if !ok {
		return r, nil, errors.New("No user provided")
	}
	if u, ok := userMap[user]; ok {
		r = r.WithContext(context.WithValue(r.Context(), userKey, u))
		return r, u, nil
	} else {
		return r, nil, errors.New("No user named " + user)
	}
}

// GetUser returns a User stored in the request's context, if one exists.
func GetUser(r *http.Request) (*User, bool) {
	val := r.Context().Value(userKey)
	if val != nil {
		u, ok := val.(*User)
		return u, ok
	}
	return nil, false
}