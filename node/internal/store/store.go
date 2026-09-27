// Package store is a small JSON document store on bbolt, one bucket per collection.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DB is an open store.
type DB struct {
	b *bolt.DB
}

// Open opens (or creates) the store file.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store dir: %w", err)
	}
	b, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	return &DB{b: b}, nil
}

// Close closes the file.
func (d *DB) Close() error { return d.b.Close() }

// Put stores v as JSON under key.
func (d *DB) Put(bucket, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store %s/%s: %w", bucket, key, err)
	}
	return d.b.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
}

// Get loads key into v and reports whether it exists.
func (d *DB) Get(bucket, key string, v any) (bool, error) {
	var data []byte
	err := d.b.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bucket)); b != nil {
			if raw := b.Get([]byte(key)); raw != nil {
				data = append([]byte{}, raw...)
			}
		}
		return nil
	})
	if err != nil || data == nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return true, fmt.Errorf("load %s/%s: %w", bucket, key, err)
	}
	return true, nil
}

// Has reports whether key exists.
func (d *DB) Has(bucket, key string) bool {
	found := false
	_ = d.b.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bucket)); b != nil {
			found = b.Get([]byte(key)) != nil
		}
		return nil
	})
	return found
}

// Delete removes key.
func (d *DB) Delete(bucket, key string) error {
	return d.b.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bucket)); b != nil {
			return b.Delete([]byte(key))
		}
		return nil
	})
}

// ErrStop ends ForEach early without an error.
var ErrStop = errors.New("stop")

// ForEach calls fn for every key of the bucket in key order.
func (d *DB) ForEach(bucket string, fn func(key string, raw []byte) error) error {
	err := d.b.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error { return fn(string(k), v) })
	})
	if errors.Is(err, ErrStop) {
		return nil
	}
	return err
}

// Modify loads key into v (zero value if missing), calls fn and stores v again, atomically.
// fn returning ErrStop leaves the document unchanged.
func Modify[T any](d *DB, bucket, key string, fn func(v *T, exists bool) error) error {
	err := d.b.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		var v T
		raw := b.Get([]byte(key))
		if raw != nil {
			if err := json.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("load %s/%s: %w", bucket, key, err)
			}
		}
		if err := fn(&v, raw != nil); err != nil {
			return err
		}
		data, err := json.Marshal(&v)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), data)
	})
	if errors.Is(err, ErrStop) {
		return nil
	}
	return err
}

// List loads all documents of a bucket.
func List[T any](d *DB, bucket string) ([]T, error) {
	var out []T
	err := d.ForEach(bucket, func(key string, raw []byte) error {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("load %s/%s: %w", bucket, key, err)
		}
		out = append(out, v)
		return nil
	})
	return out, err
}
