package costledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type persisted struct {
	Version  int                      `json:"version"`
	Accounts map[string]*accountState `json:"accounts"`
}

func (l *Ledger) load() error {
	info, err := os.Lstat(l.opts.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > int64(l.opts.MaxStateBytes) {
		return errors.New("invalid ledger file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return errors.New("ledger file must be private")
	}
	raw, err := os.ReadFile(l.opts.Path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var saved persisted
	if err = d.Decode(&saved); err != nil {
		return errors.New("invalid ledger JSON")
	}
	var extra interface{}
	if d.Decode(&extra) != io.EOF || saved.Version != 1 || saved.Accounts == nil {
		return errors.New("invalid ledger version or trailing data")
	}
	for id, a := range saved.Accounts {
		if id == "" || a == nil || a.TrackingSince.IsZero() || a.Samples < 0 {
			return errors.New("invalid ledger account state")
		}
		for _, v := range []float64{a.SpentUSD, a.PeriodSpentUSD, a.AnchorUSD, a.WeightedUSD, a.WeightedFraction} {
			if !finite(v) || v < 0 {
				return errors.New("invalid ledger numeric state")
			}
		}
		if a.AnchorUSD > a.SpentUSD || a.AnchorUnpriced > a.Unpriced || a.AnchorIncomplete > a.Incomplete || a.AnchorTainted > a.Tainted || a.PeriodSpentUSD > a.SpentUSD {
			return errors.New("inconsistent ledger counters")
		}
		if (a.Latest == nil) != (a.Anchor == nil) {
			return errors.New("inconsistent ledger anchor")
		}
		if a.Latest != nil && (!validCredit(*a.Latest) || !validCredit(*a.Anchor) || a.Anchor.At.After(a.Latest.At) || epochChanged(a.Anchor, a.Latest)) {
			return errors.New("invalid ledger credit state")
		}
		if a.Samples > 0 && (a.LastSample.IsZero() || a.WeightedFraction <= 0 || a.WeightedUSD <= 0 || a.Latest == nil || a.LastSample.After(a.Latest.At)) {
			return errors.New("invalid ledger estimator")
		}
		a.Seen = map[string]bool{}
		for _, requestID := range a.RecentIDs {
			if requestID == "" || a.Seen[requestID] {
				return errors.New("invalid ledger dedup state")
			}
			a.Seen[requestID] = true
		}
		if len(a.RecentIDs) > l.opts.MaxRequestIDs {
			a.RecentIDs = a.RecentIDs[len(a.RecentIDs)-l.opts.MaxRequestIDs:]
			a.Seen = map[string]bool{}
			for _, id := range a.RecentIDs {
				a.Seen[id] = true
			}
		}
	}
	l.accounts = saved.Accounts
	return nil
}

// Flush serializes and writes ONLY outside request calls. An unreadable or
// corrupt input is never overwritten with an apparently complete empty ledger.
func (l *Ledger) Flush() error {
	if l.opts.Path == "" {
		return nil
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.mu.Lock()
	if l.loadErr != nil {
		err := l.loadErr
		l.mu.Unlock()
		return err
	}
	raw, err := json.Marshal(persisted{Version: 1, Accounts: l.accounts})
	l.mu.Unlock()
	if err == nil && len(raw)+1 > l.opts.MaxStateBytes {
		err = errors.New("ledger exceeds persistence size limit")
	}
	if err == nil {
		err = l.writeFile(append(raw, '\n'))
	}
	l.mu.Lock()
	l.saveErr = err
	l.mu.Unlock()
	return err
}

func (l *Ledger) writeFile(raw []byte) error {
	directory := filepath.Dir(l.opts.Path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(l.opts.Path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("ledger destination is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(directory, ".costledger-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, l.opts.Path); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		d, openErr := os.Open(directory)
		if openErr != nil {
			return openErr
		}
		syncErr := d.Sync()
		closeErr := d.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	return nil
}

func (l *Ledger) writer() {
	defer close(l.done)
	var timer *time.Timer
	var tick <-chan time.Time
	for {
		select {
		case <-l.changed:
			if timer == nil {
				timer = time.NewTimer(l.opts.Debounce)
				tick = timer.C
			}
		case <-tick:
			err := l.Flush()
			timer = nil
			tick = nil
			// Failed persistence must recover even if there are NO later
			// requests. The bounded fixed retry is quiet and runs only here.
			if err != nil {
				timer = time.NewTimer(l.opts.Debounce)
				tick = timer.C
			}
		case <-l.stop:
			if timer != nil {
				timer.Stop()
			}
			_ = l.Flush()
			return
		}
	}
}
