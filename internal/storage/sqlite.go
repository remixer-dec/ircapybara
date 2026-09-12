package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/glebarez/go-sqlite"

	"ircapybara/internal/config"
	"ircapybara/internal/model"
	"ircapybara/internal/protocol"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS identities (
            nick_norm TEXT PRIMARY KEY,
            nick_display TEXT NOT NULL,
            salt BLOB NOT NULL,
            pass_hash BLOB NOT NULL,
            created_at INTEGER NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS channels (
            name_norm TEXT PRIMARY KEY,
            name_display TEXT NOT NULL,
            topic TEXT NOT NULL,
            created_at INTEGER NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS bans (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            scope TEXT NOT NULL,
            channel_norm TEXT NOT NULL DEFAULT '',
            kind TEXT NOT NULL,
            value TEXT NOT NULL,
            created_at INTEGER NOT NULL,
            UNIQUE(scope, channel_norm, kind, value)
        )`,
	}
	for _, q := range schema {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func (s *Store) SeedChannels(channels []config.SeedChannel) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	for _, ch := range channels {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO channels(name_norm,name_display,topic,created_at) VALUES(?,?,?,?)`, protocol.Norm(ch.Name), ch.Name, ch.Topic, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadChannels() (map[string]*model.Channel, error) {
	rows, err := s.db.Query(`SELECT name_norm,name_display,topic FROM channels ORDER BY name_norm`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*model.Channel)
	for rows.Next() {
		var norm, name, topic string
		if err := rows.Scan(&norm, &name, &topic); err != nil {
			return nil, err
		}
		out[norm] = &model.Channel{Name: name, Norm: norm, Topic: topic, Members: make(map[*model.Client]*model.Membership), Invited: make(map[*model.Client]bool)}
	}
	return out, rows.Err()
}

func (s *Store) AddChannel(name, topic string) error {
	_, err := s.db.Exec(`INSERT INTO channels(name_norm,name_display,topic,created_at) VALUES(?,?,?,?)`, protocol.Norm(name), name, topic, time.Now().Unix())
	return err
}

func (s *Store) SetTopic(nameNorm, topic string) error {
	res, err := s.db.Exec(`UPDATE channels SET topic=? WHERE name_norm=?`, topic, nameNorm)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("channel not found")
	}
	return nil
}

func (s *Store) LoadIdentities() (map[string]model.Credential, error) {
	rows, err := s.db.Query(`SELECT nick_norm,nick_display,salt,pass_hash FROM identities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]model.Credential)
	for rows.Next() {
		var norm, display string
		var salt, hash []byte
		if err := rows.Scan(&norm, &display, &salt, &hash); err != nil {
			return nil, err
		}
		out[norm] = model.Credential{NickDisplay: display, Salt: append([]byte(nil), salt...), Hash: append([]byte(nil), hash...)}
	}
	return out, rows.Err()
}

func (s *Store) RegisterIdentity(nick string, salt, hash []byte) error {
	_, err := s.db.Exec(`INSERT INTO identities(nick_norm,nick_display,salt,pass_hash,created_at) VALUES(?,?,?,?,?)`, protocol.Norm(nick), nick, salt, hash, time.Now().Unix())
	return err
}

func (s *Store) LoadBans() ([]model.Ban, error) {
	rows, err := s.db.Query(`SELECT id,scope,channel_norm,kind,value FROM bans ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Ban
	for rows.Next() {
		var b model.Ban
		if err := rows.Scan(&b.ID, &b.Scope, &b.ChannelNorm, &b.Kind, &b.Value); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) AddBan(scope, channelNorm, kind, value string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO bans(scope,channel_norm,kind,value,created_at) VALUES(?,?,?,?,?)`, scope, channelNorm, kind, value, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) DeleteBan(scope, channelNorm, kind, value string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM bans WHERE scope=? AND channel_norm=? AND kind=? AND value=?`, scope, channelNorm, kind, value)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
