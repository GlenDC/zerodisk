package ardb

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/zero-os/0-Disk/log"
	"github.com/zero-os/0-Disk/redisstub"
)

// use pool in testRedisProvider
// as we want to trigger deadlock in pools

func newTestRedisPool(dial func() (redis.Conn, error)) *redis.Pool {
	return &redis.Pool{
		MaxActive:   10,
		MaxIdle:     10,
		Wait:        true,
		IdleTimeout: 5 * time.Second,
		Dial:        dial,
	}
}

func newTestRedisProvider(primary, template *redisstub.MemoryRedis) *testRedisProvider {
	provider := &testRedisProvider{
		memRedis:         primary,
		templateMemRedis: template,
	}
	provider.primaryPool = newTestRedisPool(func() (redis.Conn, error) {
		if provider.memRedis == nil {
			return nil, errors.New("no memory redis available")
		}

		return provider.memRedis.Dial("", 0)
	})
	provider.templatePool = newTestRedisPool(func() (redis.Conn, error) {
		if provider.templateMemRedis == nil {
			return nil, errors.New("no template memory redis available")
		}

		return provider.templateMemRedis.Dial("", 0)
	})
	return provider
}

type testRedisProvider struct {
	memRedis                  *redisstub.MemoryRedis
	templateMemRedis          *redisstub.MemoryRedis
	primaryPool, templatePool *redis.Pool
}

func (trp *testRedisProvider) RedisConnection(index int64) (redis.Conn, error) {
	return trp.primaryPool.Get(), nil
}

func (trp *testRedisProvider) MetaRedisConnection() (redis.Conn, error) {
	return trp.primaryPool.Get(), nil
}

func (trp *testRedisProvider) TemplateRedisConnection(index int64) (redis.Conn, error) {
	return trp.templatePool.Get(), nil
}

func (trp *testRedisProvider) Close() error {
	trp.memRedis.Close()
	trp.templateMemRedis.Close()
	return nil
}

// shared test function to test all types of BlockStorage equally,
// this gives us some confidence that all storages behave the same
// from an end-user perspective
func testBackendStorage(t *testing.T, storage BlockStorage) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go storage.GoBackground(ctx)
	defer storage.Close()

	var (
		testContentA = []byte{4, 2}
	)
	const (
		testBlockIndexA = 0
		testBlockIndexB = 1
	)

	// deleting non-existing content is fine
	// this just results in a noop
	err := storage.Delete(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}

	// getting non-existing content,
	// is not an error, and results in nil-content
	content, err := storage.Get(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}

	// setting content should be always fine
	for i := 0; i < 3; i++ {
		err = storage.Set(testBlockIndexA, testContentA)
		if err != nil {
			t.Fatal(err)
		}
	}

	// getting this content should now be possible
	content, err = storage.Get(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) < 2 || bytes.Compare(testContentA, content[:2]) != 0 {
		t.Fatalf("unexpected content found: %v", content)
	}

	// deleting and getting non-existent content is still fine
	err = storage.Delete(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	// getting the content should now fail
	content, err = storage.Get(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}

	// Deleting content, should really delete it
	err = storage.Delete(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}

	// content should now be nil
	content, err = storage.Get(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}

	// Deleting content, should really delete it
	err = storage.Delete(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}

	// content should now be nil
	content, err = storage.Get(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}
}

// shared test function to test all types of BlockStorage equally,
// this gives us some confidence that all storages behave the same
// from an end-user perspective
func testBackendStorageForceFlush(t *testing.T, storage BlockStorage) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go storage.GoBackground(ctx)
	defer storage.Close()

	var (
		testContentA = []byte{4, 2}
	)
	const (
		testBlockIndexA = 0
		testBlockIndexB = 1
	)

	// deleting non-existing content is fine
	// this just results in a noop
	err := storage.Delete(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}

	// getting non-existing content,
	// is not an error, and results in nil-content
	content, err := storage.Get(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}

	// setting content, and getting set content should be always fine
	for i := 0; i < 3; i++ {
		err = storage.Set(testBlockIndexA, testContentA)
		if err != nil {
			t.Fatal(i, err)
		}

		// getting this content should be possible
		content, err = storage.Get(testBlockIndexA)
		if err != nil {
			t.Fatal(i, err)
		}
		if len(content) < 2 || bytes.Compare(testContentA, content[:2]) != 0 {
			t.Fatalf("iteration %d: unexpected content found: %v", i, content)
		}

		err = storage.Flush()
		if err != nil {
			t.Fatal(i, err)
		}

		// getting this content should still be possible
		content, err = storage.Get(testBlockIndexA)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) < 2 || bytes.Compare(testContentA, content[:2]) != 0 {
			t.Fatalf("iteration %d: unexpected content found: %v", i, content)
		}
	}

	// deleting and getting non-existent content is still fine
	err = storage.Delete(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	// getting the content should now fail
	content, err = storage.Get(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	err = storage.Flush()
	if err != nil {
		t.Fatal(err)
	}
	// getting the content should now still fail
	content, err = storage.Get(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}

	// Deleting content, should really delete it
	err = storage.Delete(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}
	err = storage.Flush()
	if err != nil {
		t.Fatal(err)
	}

	// content should now be nil
	content, err = storage.Get(testBlockIndexA)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}

	// Deleting content, should really delete it
	err = storage.Delete(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}

	// content should now be nil
	content, err = storage.Get(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}

	err = storage.Flush()
	if err != nil {
		t.Fatal(err)
	}

	// content should still be nil
	content, err = storage.Get(testBlockIndexB)
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		t.Fatalf("found content %v, while expected nil-content", content)
	}
}

// shared test function to test all types of BlockStorage equally,
// and make sure they don't get in a deadlock situation, after being used for a while.
// test in a response to https://github.com/zero-os/0-Disk/issues/89
func testBackendStorageDeadlock(t *testing.T, blockSize, blockCount int64, storage BlockStorage) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go storage.GoBackground(ctx)
	defer storage.Close()

	// store random content eight times
	// each time we do all storage async at once,
	// and wait for them all to be done
	for time := int64(0); time < 8; time++ {
		var wg sync.WaitGroup

		for i := int64(0); i < blockCount; i++ {
			wg.Add(1)

			preContent := make([]byte, blockSize)
			rand.Read(preContent)

			go func(blockIndex int64) {
				defer wg.Done()

				// set content
				if err := storage.Set(blockIndex, preContent); err != nil {
					t.Fatal(time, blockIndex, err)
					return
				}

				// get content
				postContent, err := storage.Get(blockIndex)
				if err != nil {
					t.Fatal(time, blockIndex, err)
					return
				}

				// make sure the postContent (GET) equals the preContent (SET)
				if bytes.Compare(preContent, postContent) != 0 {
					t.Fatal(time, blockIndex, "unexpected content received", preContent, postContent)
				}
			}(i)
		}

		wg.Wait()

		// let's flush each time
		if err := storage.Flush(); err != nil {
			t.Fatal(time, err)
		}
	}

	var wg sync.WaitGroup

	// delete all content (async)
	for i := int64(0); i < blockCount; i++ {
		blockIndex := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := storage.Delete(blockIndex); err != nil {
				t.Fatal(blockIndex, err)
				return
			}

			// content should now be gone

			postContent, err := storage.Get(blockIndex)
			if err != nil {
				t.Fatal(blockIndex, err)
				return
			}

			if len(postContent) != 0 {
				t.Errorf("didn't expect to find content for #%d: %v", blockIndex, postContent)
			}
		}()
	}

	wg.Wait()

	// let's flush the deleted content
	if err := storage.Flush(); err != nil {
		t.Fatal(err)
	}

	// getting all the content should still fail
	for i := int64(0); i < blockCount; i++ {
		blockIndex := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			postContent, err := storage.Get(blockIndex)
			if err != nil {
				t.Fatal(blockIndex, err)
				return
			}

			if len(postContent) != 0 {
				t.Errorf("didn't expect to find content for #%d: %v", blockIndex, postContent)
			}
		}()
	}

	wg.Wait()
}

func init() {
	log.SetLevel(log.DebugLevel)
}
