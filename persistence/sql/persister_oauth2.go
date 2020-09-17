package sql

import (
	"context"
	"crypto/sha512"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gobuffalo/pop/v5"
	"github.com/pkg/errors"
	"github.com/tidwall/gjson"

	"github.com/ory/fosite"
	"github.com/ory/herodot"
	"github.com/ory/hydra/oauth2"
	"github.com/ory/x/sqlcon"
	"github.com/ory/x/stringsx"
)

var _ oauth2.AssertionJWTReader = &Persister{}

type (
	tableName  string
	RequestSQL struct {
		ID                string         `db:"signature"`
		Request           string         `db:"request_id"`
		ConsentChallenge  sql.NullString `db:"challenge_id"`
		RequestedAt       time.Time      `db:"requested_at"`
		Client            string         `db:"client_id"`
		Scopes            string         `db:"scope"`
		GrantedScope      string         `db:"granted_scope"`
		RequestedAudience string         `db:"requested_audience"`
		GrantedAudience   string         `db:"granted_audience"`
		Form              string         `db:"form_data"`
		Subject           string         `db:"subject"`
		Active            bool           `db:"active"`
		Session           []byte         `db:"session_data"`
		table             tableName      `db:"-"`
	}
)

const (
	sqlTableOpenID         tableName = "oidc"
	sqlTableAccess         tableName = "access"
	sqlTableRefresh        tableName = "refresh"
	sqlTableCode           tableName = "code"
	sqlTablePKCE           tableName = "pkce"
)

func (r RequestSQL) TableName() string {
	return "hydra_oauth2_" + string(r.table)
}

func (r *RequestSQL) toRequest(ctx context.Context, session fosite.Session, p *Persister) (*fosite.Request, error) {
	sess := r.Session
	if !gjson.ValidBytes(sess) {
		var err error
		sess, err = p.r.KeyCipher().Decrypt(string(sess))
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	if session != nil {
		if err := json.Unmarshal(sess, session); err != nil {
			return nil, errors.WithStack(err)
		}
	} else {
		p.l.Debugf("Got an empty session in toRequest")
	}

	c, err := p.GetClient(ctx, r.Client)
	if err != nil {
		return nil, err
	}

	val, err := url.ParseQuery(r.Form)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &fosite.Request{
		ID:                r.Request,
		RequestedAt:       r.RequestedAt,
		Client:            c,
		RequestedScope:    stringsx.Splitx(r.Scopes, "|"),
		GrantedScope:      stringsx.Splitx(r.GrantedScope, "|"),
		RequestedAudience: stringsx.Splitx(r.RequestedAudience, "|"),
		GrantedAudience:   stringsx.Splitx(r.GrantedAudience, "|"),
		Form:              val,
		Session:           session,
	}, nil
}

// hashSignature prevents errors where the signature is longer than 128 characters (and thus doesn't fit into the pk).
func (p *Persister) hashSignature(signature string, table tableName) string {
	if table == sqlTableAccess && p.config.IsUsingJWTAsAccessTokens() {
		return fmt.Sprintf("%x", sha512.Sum384([]byte(signature)))
	}
	return signature
}

func (p *Persister) sqlSchemaFromRequest(rawSignature string, r fosite.Requester, table tableName) (*RequestSQL, error) {
	subject := ""
	if r.GetSession() == nil {
		p.l.Debugf("Got an empty session in sqlSchemaFromRequest")
	} else {
		subject = r.GetSession().GetSubject()
	}

	session, err := json.Marshal(r.GetSession())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if p.config.EncryptSessionData() {
		ciphertext, err := p.r.KeyCipher().Encrypt(session)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		session = []byte(ciphertext)
	}

	var challenge sql.NullString
	rr, ok := r.GetSession().(*oauth2.Session)
	if !ok && r.GetSession() != nil {
		return nil, errors.Errorf("Expected request to be of type *Session, but got: %T", r.GetSession())
	} else if ok {
		if len(rr.ConsentChallenge) > 0 {
			challenge = sql.NullString{Valid: true, String: rr.ConsentChallenge}
		}
	}

	return &RequestSQL{
		Request:           r.GetID(),
		ConsentChallenge:  challenge,
		ID:                p.hashSignature(rawSignature, table),
		RequestedAt:       r.GetRequestedAt(),
		Client:            r.GetClient().GetID(),
		Scopes:            strings.Join(r.GetRequestedScopes(), "|"),
		GrantedScope:      strings.Join(r.GetGrantedScopes(), "|"),
		GrantedAudience:   strings.Join(r.GetGrantedAudience(), "|"),
		RequestedAudience: strings.Join(r.GetRequestedAudience(), "|"),
		Form:              r.GetRequestForm().Encode(),
		Session:           session,
		Subject:           subject,
		Active:            true,
		table:             table,
	}, nil
}

func (p *Persister) GetClientAssertionJWT(ctx context.Context, j string) (*oauth2.BlacklistedJTI, error) {
	jti := oauth2.NewBlacklistedJTI(j, time.Time{})
	return jti, sqlcon.HandleError(p.Connection(ctx).Find(jti, jti.ID))
}

func (p *Persister) SetClientAssertionJWTRaw(ctx context.Context, jti *oauth2.BlacklistedJTI) error {
	return sqlcon.HandleError(p.Connection(ctx).Create(jti))
}

func (p *Persister) ClientAssertionJWTValid(ctx context.Context, jti string) error {
	j, err := p.GetClientAssertionJWT(ctx, jti)
	if errors.Is(err, sqlcon.ErrNoRows) {
		// the jti is not known => valid
		return nil
	} else if err != nil {
		return err
	}
	if j.Expiry.After(time.Now()) {
		// the jti is not expired yet => invalid
		return errors.WithStack(fosite.ErrJTIKnown)
	}
	// the jti is expired => valid
	return nil
}

func (p *Persister) SetClientAssertionJWT(ctx context.Context, jti string, exp time.Time) error {
	return p.transaction(ctx, func(ctx context.Context, c *pop.Connection) error {
		// delete expired
		now := "now()"
		if c.Dialect.Name() == "sqlite3" {
			now = "CURRENT_TIMESTAMP"
		}
		if err := c.RawQuery(fmt.Sprintf("DELETE FROM %s WHERE expires_at < %s", oauth2.BlacklistedJTI{}.TableName(), now)).Exec(); err != nil {
			return sqlcon.HandleError(err)
		}

		if err := p.SetClientAssertionJWTRaw(ctx, oauth2.NewBlacklistedJTI(jti, exp)); errors.Is(err, sqlcon.ErrUniqueViolation) {
			// found a jti
			return errors.WithStack(fosite.ErrJTIKnown)
		} else if err != nil {
			return err
		}
		// setting worked without a problem
		return nil
	})
}

func (p *Persister) createSession(ctx context.Context, signature string, requester fosite.Requester, table tableName) error {
	req, err := p.sqlSchemaFromRequest(signature, requester, table)
	if err != nil {
		return err
	}

	if err := p.Connection(ctx).Create(req); err != nil {
		err = sqlcon.HandleError(err)
		switch errCause := errors.Cause(err).(type) {
		case *herodot.DefaultError:
			if errCause == sqlcon.ErrConcurrentUpdate {
				return errors.Wrap(fosite.ErrSerializationFailure, err.Error())
			}
		}
		return err
	}

	return nil
}

func (p *Persister) findSessionBySignature(ctx context.Context, rawSignature string, session fosite.Session, table tableName) (fosite.Requester, error) {
	rawSignature = p.hashSignature(rawSignature, table)

	var fr fosite.Requester
	return fr, p.transaction(ctx, func(ctx context.Context, c *pop.Connection) (err error) {
		r := RequestSQL{table: table}

		err = p.Connection(ctx).Where("signature = ?", rawSignature).First(&r)
		if err == sql.ErrNoRows {
			return errors.WithStack(fosite.ErrNotFound)
		} else if err != nil {
			return sqlcon.HandleError(err)
		} else if !r.Active && table == sqlTableCode {
			fr, err = r.toRequest(ctx, session, p)
			if err != nil {
				return err
			} else {
				return errors.WithStack(fosite.ErrInvalidatedAuthorizeCode)
			}
		} else if !r.Active {
			return errors.WithStack(fosite.ErrInactiveToken)
		}

		fr, err = r.toRequest(ctx, session, p)
		return
	})
}

func (p *Persister) deleteSession(ctx context.Context, signature string, table tableName) error {
	signature = p.hashSignature(signature, table)

	return sqlcon.HandleError(
		p.Connection(ctx).
			RawQuery(fmt.Sprintf("DELETE FROM %s WHERE signature=?", RequestSQL{table: table}.TableName()), signature).
			Exec())
}

func (p *Persister) revokeSession(ctx context.Context, id string, table tableName) error {
	if err := p.Connection(ctx).RawQuery(
		fmt.Sprintf("DELETE FROM %s WHERE request_id=?", RequestSQL{table: table}.TableName()),
		id,
	).Exec(); err == sql.ErrNoRows {
		return errors.WithStack(fosite.ErrNotFound)
	} else if err := sqlcon.HandleError(err); err != nil {
		switch errCause := errors.Cause(err).(type) {
		case *herodot.DefaultError:
			if errCause == sqlcon.ErrConcurrentUpdate {
				return errors.Wrap(fosite.ErrSerializationFailure, err.Error())
			}
		}
		return err
	}
	return nil
}

func (p *Persister) CreateAuthorizeCodeSession(ctx context.Context, signature string, requester fosite.Requester) (err error) {
	return p.createSession(ctx, signature, requester, sqlTableCode)
}

func (p *Persister) GetAuthorizeCodeSession(ctx context.Context, signature string, session fosite.Session) (request fosite.Requester, err error) {
	return p.findSessionBySignature(ctx, signature, session, sqlTableCode)
}

func (p *Persister) InvalidateAuthorizeCodeSession(ctx context.Context, signature string) (err error) {
	return sqlcon.HandleError(
		p.Connection(ctx).
			RawQuery(
				fmt.Sprintf("UPDATE %s SET active=false WHERE signature=?", RequestSQL{table: sqlTableCode}.TableName()),
				signature).
			Exec())
}

func (p *Persister) CreateAccessTokenSession(ctx context.Context, signature string, requester fosite.Requester) (err error) {
	return p.createSession(ctx, signature, requester, sqlTableAccess)
}

func (p *Persister) GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (request fosite.Requester, err error) {
	return p.findSessionBySignature(ctx, signature, session, sqlTableAccess)
}

func (p *Persister) DeleteAccessTokenSession(ctx context.Context, signature string) (err error) {
	return p.deleteSession(ctx, signature, sqlTableAccess)
}

func (p *Persister) CreateRefreshTokenSession(ctx context.Context, signature string, requester fosite.Requester) (err error) {
	return p.createSession(ctx, signature, requester, sqlTableRefresh)
}

func (p *Persister) GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (request fosite.Requester, err error) {
	return p.findSessionBySignature(ctx, signature, session, sqlTableRefresh)
}

func (p *Persister) DeleteRefreshTokenSession(ctx context.Context, signature string) (err error) {
	return p.deleteSession(ctx, signature, sqlTableRefresh)
}

func (p *Persister) CreateOpenIDConnectSession(ctx context.Context, signature string, requester fosite.Requester) error {
	return p.createSession(ctx, signature, requester, sqlTableOpenID)
}

func (p *Persister) GetOpenIDConnectSession(ctx context.Context, signature string, requester fosite.Requester) (fosite.Requester, error) {
	return p.findSessionBySignature(ctx, signature, requester.GetSession(), sqlTableOpenID)
}

func (p *Persister) DeleteOpenIDConnectSession(ctx context.Context, signature string) error {
	return p.deleteSession(ctx, signature, sqlTableOpenID)
}

func (p *Persister) GetPKCERequestSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	return p.findSessionBySignature(ctx, signature, session, sqlTablePKCE)
}

func (p *Persister) CreatePKCERequestSession(ctx context.Context, signature string, requester fosite.Requester) error {
	return p.createSession(ctx, signature, requester, sqlTablePKCE)
}

func (p *Persister) DeletePKCERequestSession(ctx context.Context, signature string) error {
	return p.deleteSession(ctx, signature, sqlTablePKCE)
}

func (p *Persister) RevokeRefreshToken(ctx context.Context, id string) error {
	return p.revokeSession(ctx, id, sqlTableRefresh)
}

func (p *Persister) RevokeAccessToken(ctx context.Context, id string) error {
	return p.revokeSession(ctx, id, sqlTableAccess)
}

func (p *Persister) FlushInactiveAccessTokens(ctx context.Context, notAfter time.Time) error {
	err := p.Connection(ctx).RawQuery(
		fmt.Sprintf("DELETE FROM %s WHERE requested_at < ? AND requested_at < ?", RequestSQL{table: sqlTableAccess}.TableName()),
		time.Now().Add(-p.config.AccessTokenLifespan()),
		notAfter,
	).Exec()
	if err == sql.ErrNoRows {
		return errors.Wrap(fosite.ErrNotFound, "")
	}
	return sqlcon.HandleError(err)
}
