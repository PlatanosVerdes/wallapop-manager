package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Muting is this service's own business and never Wallapop's: the alert switch in the app
// stays where its owner left it, and this only says which of the searches it watches are
// worth a message today. So a search can be on in the app and silent here, and nothing
// that happens here is visible from the phone's Wallapop.
type Mutes struct {
	Muted map[string]Mute `json:"muted"`

	mu   sync.RWMutex
	path string
}

type Mute struct {
	// Name is kept so a list of what is silenced can be read without asking Wallapop.
	Name  string    `json:"name"`
	Since time.Time `json:"since"`
}

func LoadMutes(dir string) (*Mutes, error) {
	mutes := &Mutes{Muted: map[string]Mute{}, path: filepath.Join(dir, "mutes.json")}
	raw, err := os.ReadFile(mutes.path)
	if os.IsNotExist(err) {
		return mutes, nil
	}
	if err != nil {
		return mutes, err
	}
	if err := json.Unmarshal(raw, mutes); err != nil {
		return mutes, err
	}
	if mutes.Muted == nil {
		mutes.Muted = map[string]Mute{}
	}
	return mutes, nil
}

func (m *Mutes) IsMuted(searchID string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.Muted[searchID]
	return ok
}

// Toggle flips one search and reports where it left it.
func (m *Mutes) Toggle(searchID, name string) (muted bool, err error) {
	m.mu.Lock()
	if _, ok := m.Muted[searchID]; ok {
		delete(m.Muted, searchID)
	} else {
		m.Muted[searchID] = Mute{Name: name, Since: time.Now()}
		muted = true
	}
	m.mu.Unlock()
	return muted, m.save()
}

func (m *Mutes) Mute(searchID, name string) error {
	m.mu.Lock()
	m.Muted[searchID] = Mute{Name: name, Since: time.Now()}
	m.mu.Unlock()
	return m.save()
}

func (m *Mutes) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.Muted)
}

func (m *Mutes) save() error {
	m.mu.RLock()
	raw, err := json.MarshalIndent(m, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
