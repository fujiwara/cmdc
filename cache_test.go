package cmdc_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/fujiwara/cmdc"
)

func chunk(s string) []cmdc.Chunk {
	return []cmdc.Chunk{{Stream: cmdc.Stdout, Data: []byte(s)}}
}

func TestCacheExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cmdc.NewCache(1024)
		if _, ok := c.Get("k", time.Minute); ok {
			t.Fatal("unexpected hit on empty cache")
		}
		if !c.Set("k", "cmd", chunk("hello"), time.Minute) {
			t.Fatal("failed to set")
		}
		time.Sleep(30 * time.Second)
		chunks, ok := c.Get("k", time.Minute)
		if !ok || string(chunks[0].Data) != "hello" {
			t.Fatalf("unexpected result: %v %v", chunks, ok)
		}
		// a shorter expires on get treats the entry as stale, but keeps it
		if _, ok := c.Get("k", 10*time.Second); ok {
			t.Error("unexpected hit with shorter expires")
		}
		if _, ok := c.Get("k", time.Minute); !ok {
			t.Error("entry must be kept")
		}
		// a longer expires on get does not extend the life of the entry
		time.Sleep(30 * time.Second)
		if _, ok := c.Get("k", time.Hour); ok {
			t.Error("unexpected hit after expiration")
		}
		if n := c.Len(); n != 0 {
			t.Errorf("expired entry must be removed: len=%d", n)
		}
	})
}

func TestCacheSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cmdc.NewCache(1024)
		c.Set("short", "short", chunk("a"), time.Second)
		c.Set("long", "long", chunk("bb"), time.Minute)
		time.Sleep(2 * time.Second)
		items, total := c.Stats()
		if len(items) != 1 || total != 2 || items[0].Command != "long" {
			t.Errorf("unexpected stats: %v %d", items, total)
		}
		c.Sweep()
		if n := c.Len(); n != 1 {
			t.Errorf("unexpected len: %d", n)
		}
		if n := c.Clear(); n != 1 {
			t.Errorf("unexpected cleared: %d", n)
		}
		if n := c.Len(); n != 0 {
			t.Errorf("unexpected len after clear: %d", n)
		}
	})
}

func TestCacheEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cmdc.NewCache(10)
		c.Set("a", "a", chunk("aaaa"), time.Minute)
		time.Sleep(time.Second)
		c.Set("b", "b", chunk("bbbb"), time.Minute)
		time.Sleep(time.Second)
		// evicts the oldest entry to make room
		if !c.Set("c", "c", chunk("cccc"), time.Minute) {
			t.Fatal("failed to set")
		}
		if _, ok := c.Get("a", time.Minute); ok {
			t.Error("the oldest entry must be evicted")
		}
		for _, k := range []string{"b", "c"} {
			if _, ok := c.Get(k, time.Minute); !ok {
				t.Errorf("%s must be kept", k)
			}
		}
		// replacing an entry does not evict others
		if !c.Set("b", "b", chunk("BBBBBB"), time.Minute) {
			t.Fatal("failed to replace")
		}
		if _, total := c.Stats(); total != 10 {
			t.Errorf("unexpected total: %d", total)
		}
		// an entry larger than the cache is rejected
		if c.Set("d", "d", chunk("ddddddddddd"), time.Minute) {
			t.Error("too large entry must be rejected")
		}
		if n := c.Len(); n != 2 {
			t.Errorf("unexpected len: %d", n)
		}
	})
}

func TestCacheKey(t *testing.T) {
	t.Setenv("CMDC_TEST_A", "1")
	base := cmdc.CacheKey([]string{"echo", "a b"}, nil)
	if base != cmdc.CacheKey([]string{"echo", "a b"}, nil) {
		t.Error("key must be stable")
	}
	if base == cmdc.CacheKey([]string{"echo", "a", "b"}, nil) {
		t.Error("key must depend on the boundaries of arguments")
	}
	withEnv := cmdc.CacheKey([]string{"echo", "a b"}, []string{"CMDC_TEST_A"})
	if base == withEnv {
		t.Error("key must depend on key-env")
	}
	t.Setenv("CMDC_TEST_A", "2")
	if withEnv == cmdc.CacheKey([]string{"echo", "a b"}, []string{"CMDC_TEST_A"}) {
		t.Error("key must depend on the value of key-env")
	}
	if base != cmdc.CacheKey([]string{"echo", "a b"}, nil) {
		t.Error("key must not depend on other environment variables")
	}
}
