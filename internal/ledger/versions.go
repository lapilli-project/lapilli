package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"lapilli.dev/internal/rules"
)

// Version is a version of the rules, in effect from Since until the next
// one's; a zero Since is from before the ledger began.
type Version struct {
	ID     int64
	Since  time.Time
	Digest string
	Docs   []rules.Doc
	// Names are the names the rules the documents hold are judged under,
	// in the order rules.FromDocs reads them.
	Names []string
}

type docRef struct {
	Source string `json:"source"`
	API    bool   `json:"api,omitempty"`
	Hash   string `json:"hash"`
}

// AddVersion keeps a version of the rules. A document already kept for an
// earlier version isn't kept twice.
func (l *Ledger) AddVersion(ctx context.Context, v Version) (int64, error) {
	var id int64
	err := l.write(ctx, func(t *txn) error {
		refs := make([]docRef, len(v.Docs))
		for i, d := range v.Docs {
			sum := sha256.Sum256(d.Data)
			refs[i] = docRef{d.Source, d.API, hex.EncodeToString(sum[:])}
			z, err := gz(d.Data)
			if err != nil {
				return err
			}
			if _, err := t.ExecContext(t.ctx, "INSERT INTO docs (hash, data) VALUES (?, ?) ON CONFLICT (hash) DO NOTHING", refs[i].Hash, z); err != nil {
				return err
			}
		}
		docs, err := json.Marshal(refs)
		if err != nil {
			return err
		}
		names, err := json.Marshal(v.Names)
		if err != nil {
			return err
		}
		if err := l.yield(t, v.Names); err != nil {
			return err
		}
		t.versioned = nil
		return t.QueryRowContext(t.ctx, "INSERT INTO versions (since, digest, docs, names) VALUES (?, ?, ?, ?) RETURNING id",
			unixOr0(v.Since), v.Digest, string(docs), string(names)).Scan(&id)
	})
	return id, err
}

// Versions are the versions in effect at some time in [from, to], the
// earliest first; with docs false, without their documents.
func (l *Ledger) Versions(ctx context.Context, from, to time.Time, docs bool) ([]Version, error) {
	rows, err := l.r.QueryContext(ctx, `SELECT id, since, digest, docs, names FROM versions
		WHERE since <= ? AND (since > ? OR id = (SELECT id FROM versions WHERE since <= ? ORDER BY since DESC, id DESC LIMIT 1))
		ORDER BY since, id`, to.Unix(), from.Unix(), from.Unix())
	if err != nil {
		return nil, err
	}
	var out []Version
	var refs [][]docRef
	for rows.Next() {
		var v Version
		var since int64
		var docText, names string
		if err := rows.Scan(&v.ID, &since, &v.Digest, &docText, &names); err != nil {
			rows.Close()
			return nil, err
		}
		if since > 0 {
			v.Since = unix(since)
		}
		var rs []docRef
		if err := json.Unmarshal([]byte(docText), &rs); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(names), &v.Names); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
		refs = append(refs, rs)
	}
	rows.Close()
	if err := rows.Err(); err != nil || !docs {
		return out, err
	}
	for i := range out {
		if out[i].Docs, err = l.docs(ctx, refs[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l *Ledger) docs(ctx context.Context, refs []docRef) ([]rules.Doc, error) {
	var out []rules.Doc
	for _, r := range refs {
		var z []byte
		if err := l.r.QueryRowContext(ctx, "SELECT data FROM docs WHERE hash = ?", r.Hash).Scan(&z); err != nil {
			return nil, err
		}
		var data []byte
		if err := ungz(z, &data); err != nil {
			return nil, err
		}
		out = append(out, rules.Doc{Source: r.Source, API: r.API, Data: data})
	}
	return out, nil
}

// VersionByID is one version, with its documents.
func (l *Ledger) VersionByID(ctx context.Context, id int64) (Version, error) {
	var v Version
	var since int64
	var docText, names string
	err := l.r.QueryRowContext(ctx, "SELECT id, since, digest, docs, names FROM versions WHERE id = ?", id).
		Scan(&v.ID, &since, &v.Digest, &docText, &names)
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("version %d of the rules: %w", id, ErrNotFound)
	}
	if err != nil {
		return v, err
	}
	if since > 0 {
		v.Since = unix(since)
	}
	var refs []docRef
	if err := json.Unmarshal([]byte(docText), &refs); err != nil {
		return v, err
	}
	if err := json.Unmarshal([]byte(names), &v.Names); err != nil {
		return v, err
	}
	v.Docs, err = l.docs(ctx, refs)
	return v, err
}

// Latest is the version in effect now; ok is false before the first.
func (l *Ledger) Latest(ctx context.Context) (v Version, ok bool, err error) {
	var since int64
	var names string
	err = l.r.QueryRowContext(ctx, "SELECT id, since, digest, names FROM versions ORDER BY since DESC, id DESC LIMIT 1").
		Scan(&v.ID, &since, &v.Digest, &names)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	if since > 0 {
		v.Since = unix(since)
	}
	return v, true, json.Unmarshal([]byte(names), &v.Names)
}
