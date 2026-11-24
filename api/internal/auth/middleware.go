package auth

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"
)

// Interceptor loads the session + device for every request and puts an Actor in
// context. It never rejects — per-RPC authorization is enforced in the handlers/
// service, so public RPCs (login, GetSession) still work with no session.
func (s *Service) Interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			h := req.Header()
			sessionTok := headerCookie(h, sessionCookie)
			if sessionTok == "" {
				sessionTok = bearerToken(h) // mobile
			}
			deviceTok := headerCookie(h, deviceCookie)
			actor := s.ResolveActor(ctx, sessionTok, deviceTok)
			return next(withActor(ctx, actor), req)
		}
	}
}

func headerCookie(h http.Header, name string) string {
	r := &http.Request{Header: h}
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func bearerToken(h http.Header) string {
	const prefix = "Bearer "
	v := h.Get("Authorization")
	if strings.HasPrefix(v, prefix) {
		return strings.TrimPrefix(v, prefix)
	}
	return ""
}
