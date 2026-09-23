// Package users keeps who may use the bot and what each of them is looking for. A user is
// a Telegram chat: the chat is where the listings go and where the commands come from.
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
	ErrUnknown       = errors.New("no estas dado de alta")
	ErrNoSuchSearch  = errors.New("esa busqueda ya no existe")
	ErrTooMany       = errors.New("ya tienes el maximo de busquedas")
	ErrAlreadyExists = errors.New("ya tienes esa misma busqueda")
)

type User struct {
	Chat string `json:"chat"`
	Name string `json:"name"`
	// Active is what lets a chat past /start.
	Active   bool      `json:"active"`
	Since    time.Time `json:"since"`
	Searches []Search  `json:"searches,omitempty"`
}

type Search struct {
	// ID is short on purpose: it travels inside callback_data, which Telegram caps at 64
	// bytes.
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Query string    `json:"query"`
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
	// read is when the file last read or written was modified. The terminal writes the
	// same file while the service runs, and a store that never looked again would save
	// over it.
	read time.Time
}

func Load(dir string) (*Store, error) {
	store := &Store{path: filepath.Join(dir, "users.json"), users: map[string]*User{}}
	if err := store.refresh(); err != nil {
		return nil, err
	}
	return store, nil
}

// refresh reads the file again when somebody else has written it since. Callers hold mu.
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

// lock takes the store and brings it up to date with the file. A file that cannot be read
// leaves what was in memory, which is the last state this process knew to be good.
func (s *Store) lock() error {
	s.mu.Lock()
	return s.refresh()
}

// Get answers a copy, so a round can walk a user's searches while a button edits them.
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

// All answers every user, active or waiting, ordered by the time they arrived.
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

// Request records a chat joining. It reports false when the chat was already known.
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

// Rename keeps the name a chat is shown by in step with Telegram's.
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

// Add stores a search for an active user, within the limit. The same query twice is
// refused: it would announce every listing twice.
func (s *Store) Add(chat, name string, query url.Values, limit int, now time.Time) (Search, error) {
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
	search := Search{ID: newID(), Name: name, Query: encoded, Added: now}
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

// SetMuted switches a search off or back on, and answers it in its new position.
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
