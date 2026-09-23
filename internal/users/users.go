// Package users stores each chat and its searches.
package users

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	ErrUnknown       = errors.New("no estás dado de alta, escribe /start")
	ErrNoSuchSearch  = errors.New("esa búsqueda ya no existe")
	ErrTooMany       = errors.New("ya tienes el máximo de búsquedas")
	ErrAlreadyExists = errors.New("ya tienes esa búsqueda")
)

type User struct {
	Chat     string    `json:"chat"`
	Name     string    `json:"name"`
	Active   bool      `json:"active"`
	Since    time.Time `json:"since"`
	Searches []Search  `json:"searches,omitempty"`
}

type Search struct {
	// ID is short: it travels in callback_data, which Telegram caps at 64 bytes.
	ID    string `json:"id"`
	Name  string `json:"name"`
	Query string `json:"query"`
	// Place is set when the town was written, not sent as a location.
	Place string    `json:"place,omitempty"`
	Muted bool      `json:"muted,omitempty"`
	Added time.Time `json:"added"`
}

func (s Search) Values() url.Values {
	values, _ := url.ParseQuery(s.Query)
	return values
}

type Store struct {
	mu    sync.Mutex
	path  string
	users map[string]*User
	// read is the file's mtime when last seen: the terminal writes it while the service runs.
	read time.Time
}

func Load(dir string) (*Store, error) {
	store := &Store{path: filepath.Join(dir, "searches.json"), users: map[string]*User{}}
	if err := store.refresh(); err != nil {
		return nil, err
	}
	return store, nil
}

// refresh expects mu to be held.
func (s *Store) refresh() error {
	info, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.ModTime().Equal(s.read) {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var list []*User
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	s.users = make(map[string]*User, len(list))
	for _, user := range list {
		s.users[user.Chat] = user
	}
	s.read = info.ModTime()
	return nil
}

// lock keeps what is in memory when the file cannot be read.
func (s *Store) lock() error {
	s.mu.Lock()
	return s.refresh()
}

// Get answers a copy, so a round can walk the searches while a button edits them.
func (s *Store) Get(chat string) (User, bool) {
	_ = s.lock()
	defer s.mu.Unlock()
	user, ok := s.users[chat]
	if !ok {
		return User{}, false
	}
	return clone(user), true
}

func (s *Store) IsActive(chat string) bool {
	user, ok := s.Get(chat)
	return ok && user.Active
}

func (s *Store) All() []User {
	_ = s.lock()
	defer s.mu.Unlock()
	out := make([]User, 0, len(s.users))
	for _, user := range s.users {
		out = append(out, clone(user))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

func (s *Store) Active() []User {
	var out []User
	for _, user := range s.All() {
		if user.Active {
			out = append(out, user)
		}
	}
	return out
}

// Request reports false when the chat was already known.
func (s *Store) Request(chat, name string, active bool, now time.Time) (bool, error) {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return false, err
	}
	defer s.mu.Unlock()
	if _, ok := s.users[chat]; ok {
		return false, nil
	}
	s.users[chat] = &User{Chat: chat, Name: name, Active: active, Since: now}
	return true, s.save()
}

func (s *Store) Rename(chat, name string) error {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return err
	}
	defer s.mu.Unlock()
	user, ok := s.users[chat]
	if !ok {
		return ErrUnknown
	}
	if name == "" || user.Name == name {
		return nil
	}
	user.Name = name
	return s.save()
}

func (s *Store) Remove(chat string) error {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return err
	}
	defer s.mu.Unlock()
	if _, ok := s.users[chat]; !ok {
		return ErrUnknown
	}
	delete(s.users, chat)
	return s.save()
}

// Add refuses the same query twice: it would announce every listing twice.
func (s *Store) Add(chat, name, place string, query url.Values, limit int, now time.Time) (Search, error) {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return Search{}, err
	}
	defer s.mu.Unlock()
	user, ok := s.users[chat]
	if !ok || !user.Active {
		return Search{}, ErrUnknown
	}
	encoded := query.Encode()
	for _, search := range user.Searches {
		if search.Query == encoded {
			return Search{}, fmt.Errorf("%w: %s", ErrAlreadyExists, search.Name)
		}
	}
	if limit > 0 && len(user.Searches) >= limit {
		return Search{}, fmt.Errorf("%w (%d)", ErrTooMany, limit)
	}
	search := Search{ID: newID(), Name: name, Place: place, Query: encoded, Added: now}
	user.Searches = append(user.Searches, search)
	return search, s.save()
}

func (s *Store) Search(chat, id string) (Search, error) {
	user, ok := s.Get(chat)
	if !ok {
		return Search{}, ErrUnknown
	}
	for _, search := range user.Searches {
		if search.ID == id {
			return search, nil
		}
	}
	return Search{}, ErrNoSuchSearch
}

func (s *Store) Delete(chat, id string) (Search, error) {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return Search{}, err
	}
	defer s.mu.Unlock()
	user, ok := s.users[chat]
	if !ok {
		return Search{}, ErrUnknown
	}
	for i, search := range user.Searches {
		if search.ID == id {
			user.Searches = append(user.Searches[:i], user.Searches[i+1:]...)
			return search, s.save()
		}
	}
	return Search{}, ErrNoSuchSearch
}

func (s *Store) RenameSearch(chat, id, name string) (Search, error) {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return Search{}, err
	}
	defer s.mu.Unlock()
	user, ok := s.users[chat]
	if !ok {
		return Search{}, ErrUnknown
	}
	for i := range user.Searches {
		if user.Searches[i].ID == id {
			user.Searches[i].Name = name
			return user.Searches[i], s.save()
		}
	}
	return Search{}, ErrNoSuchSearch
}

// SetQuery gives the search a new id, so its first round is silent again.
func (s *Store) SetQuery(chat, id, place string, query url.Values) (Search, error) {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return Search{}, err
	}
	defer s.mu.Unlock()
	user, ok := s.users[chat]
	if !ok {
		return Search{}, ErrUnknown
	}
	encoded := query.Encode()
	for _, search := range user.Searches {
		if search.Query == encoded && search.ID != id {
			return Search{}, fmt.Errorf("%w: %s", ErrAlreadyExists, search.Name)
		}
	}
	for i := range user.Searches {
		if user.Searches[i].ID == id {
			user.Searches[i].ID = newID()
			user.Searches[i].Query = encoded
			user.Searches[i].Place = place
			return user.Searches[i], s.save()
		}
	}
	return Search{}, ErrNoSuchSearch
}

func (s *Store) SetMuted(chat, id string, muted bool) (Search, error) {
	if err := s.lock(); err != nil {
		s.mu.Unlock()
		return Search{}, err
	}
	defer s.mu.Unlock()
	user, ok := s.users[chat]
	if !ok {
		return Search{}, ErrUnknown
	}
	for i := range user.Searches {
		if user.Searches[i].ID == id {
			user.Searches[i].Muted = muted
			return user.Searches[i], s.save()
		}
	}
	return Search{}, ErrNoSuchSearch
}

func (s *Store) save() error {
	list := make([]*User, 0, len(s.users))
	for _, user := range s.users {
		list = append(list, user)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Since.Before(list[j].Since) })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if info, err := os.Stat(s.path); err == nil {
		s.read = info.ModTime()
	}
	return nil
}

func clone(user *User) User {
	out := *user
	out.Searches = append([]Search(nil), user.Searches...)
	return out
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
