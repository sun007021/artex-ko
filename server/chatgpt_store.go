package server

import (
	"context"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/siwc"
)

// profileOAuthStore 는 siwc.Store 를 llm_profile_oauth 행에 연결한다. 토큰 상태를 이 행에 쓰는 것은
// siwc.Session 뿐이다(서버 코드는 직접 쓰지 않는다 — 프로필 삭제 시의 CASCADE 제외).
type profileOAuthStore struct {
	pg *db.DB
	id int64
}

func (st profileOAuthStore) Load(ctx context.Context) (*siwc.Record, error) {
	o, err := st.pg.GetProfileOAuth(ctx, st.id)
	if err != nil || o == nil {
		return nil, err
	}
	return &siwc.Record{
		ClientID: o.ClientID, Subject: o.Subject, Email: o.Email, IDToken: o.IDToken,
		AccessToken: o.AccessToken, RefreshToken: o.RefreshToken, ExpiresAt: o.ExpiresAt,
		EarliestRefreshAt: o.EarliestRefreshAt, Scopes: strings.Fields(o.Scopes),
		PendingIDToken: o.PendingIDToken, PendingReceivedAt: o.PendingReceivedAt,
		Status: o.Status, LastError: o.LastError,
	}, nil
}

func (st profileOAuthStore) Save(ctx context.Context, r *siwc.Record) error {
	return st.pg.SaveProfileOAuth(ctx, &db.ProfileOAuth{
		ProfileID: st.id, ClientID: r.ClientID, Subject: r.Subject, Email: r.Email, IDToken: r.IDToken,
		AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: r.ExpiresAt,
		EarliestRefreshAt: r.EarliestRefreshAt, Scopes: strings.Join(r.Scopes, " "),
		PendingIDToken: r.PendingIDToken, PendingReceivedAt: r.PendingReceivedAt,
		Status: r.Status, LastError: r.LastError,
	})
}
