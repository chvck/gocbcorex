package gocbcorex

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/couchbase/gocbcorex/testutils"
	"go.uber.org/zap"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKvClientPoolGetClient(t *testing.T) {
	mock := &KvClientMock{}
	clientConfig := KvClientConfig{
		Address:        "endpoint1",
		TlsConfig:      nil,
		SelectedBucket: "test",
		Authenticator: &PasswordAuthenticator{
			Username: "username",
			Password: "password",
		},
	}
	pool, err := NewKvClientPool(&KvClientPoolConfig{
		NumConnections: 1,
		ClientConfig:   clientConfig,
	}, &KvClientPoolOptions{
		NewKvClient: func(ctx context.Context, config *KvClientConfig) (KvClient, error) {
			assert.Equal(t, &clientConfig, config)

			return mock, nil
		},
	})
	require.NoError(t, err)

	// This is done twice to test different codepaths, this is maybe testing implementation detail a bit.
	cli, err := pool.GetClient(context.Background())
	require.NoError(t, err)

	assert.Equal(t, mock, cli)

	cli, err = pool.GetClient(context.Background())
	require.NoError(t, err)

	assert.Equal(t, mock, cli)
}

func TestKvClientPoolGetClientConcurrent(t *testing.T) {
	mock := &KvClientMock{}
	clientConfig := KvClientConfig{
		Address:        "endpoint1",
		TlsConfig:      nil,
		SelectedBucket: "test",
		Authenticator: &PasswordAuthenticator{
			Username: "username",
			Password: "password",
		},
	}
	pool, err := NewKvClientPool(&KvClientPoolConfig{
		NumConnections: 1,
		ClientConfig:   clientConfig,
	}, &KvClientPoolOptions{
		NewKvClient: func(ctx context.Context, config *KvClientConfig) (KvClient, error) {
			assert.Equal(t, &clientConfig, config)

			return mock, nil
		},
	})
	require.NoError(t, err)

	var wait sync.WaitGroup
	for i := 0; i < 50; i++ {
		wait.Add(1)
		go func() {
			cli, err := pool.GetClient(context.Background())
			assert.NoError(t, err)
			assert.Equal(t, mock, cli)

			wait.Done()
		}()
	}
	wait.Wait()
}

func TestKvClientPoolCreates5Connections(t *testing.T) {
	clientConfig := KvClientConfig{
		Address:        "endpoint1",
		TlsConfig:      nil,
		SelectedBucket: "test",
		Authenticator: &PasswordAuthenticator{
			Username: "username",
			Password: "password",
		},
	}
	var called uint32
	pool, err := NewKvClientPool(&KvClientPoolConfig{
		NumConnections: 5,
		ClientConfig:   clientConfig,
	}, &KvClientPoolOptions{
		NewKvClient: func(ctx context.Context, config *KvClientConfig) (KvClient, error) {
			assert.Equal(t, &clientConfig, config)

			atomic.AddUint32(&called, 1)

			return &KvClientMock{}, nil
		},
	})
	require.NoError(t, err)

	// This should basically be instant.
	assert.Eventually(t, func() bool {
		return atomic.LoadUint32(&called) == uint32(5)
	}, 50*time.Millisecond, 1*time.Millisecond)

	_, err = pool.GetClient(context.Background())
	require.NoError(t, err)
}

func TestKvClientPoolReconfigure(t *testing.T) {
	mock := &KvClientMock{
		ReconfigureFunc: func(opts *KvClientConfig, cb func(error)) error {
			cb(nil)
			return nil
		},
		CloseFunc: func() error { return nil },
	}
	clientConfig := KvClientConfig{
		Address:        "endpoint1",
		TlsConfig:      nil,
		SelectedBucket: "test",
		Authenticator: &PasswordAuthenticator{
			Username: "username",
			Password: "password",
		},
	}

	pool, err := NewKvClientPool(&KvClientPoolConfig{
		NumConnections: 3,
		ClientConfig:   clientConfig,
	}, &KvClientPoolOptions{
		NewKvClient: func(ctx context.Context, config *KvClientConfig) (KvClient, error) {
			assert.Equal(t, &clientConfig, config)

			return mock, nil
		},
	})
	require.NoError(t, err)

	cli, err := pool.GetClient(context.Background())
	require.NoError(t, err)

	assert.Equal(t, mock, cli)

	err = pool.Reconfigure(&KvClientPoolConfig{
		NumConnections: 1,
		ClientConfig:   clientConfig,
	}, func(error) {})
	require.NoError(t, err)

	cli, err = pool.GetClient(context.Background())
	require.NoError(t, err)

	assert.Equal(t, mock, cli)
}

func TestKvClientPoolNewAndGetRace(t *testing.T) {
	clientConfig := KvClientConfig{
		Address:        "endpoint1",
		TlsConfig:      nil,
		SelectedBucket: "test",
		Authenticator: &PasswordAuthenticator{
			Username: "username",
			Password: "password",
		},
	}
	expectedErr := errors.New("connect failure")
	pool, err := NewKvClientPool(&KvClientPoolConfig{
		NumConnections: 1,
		ClientConfig:   clientConfig,
	}, &KvClientPoolOptions{
		NewKvClient: func(ctx context.Context, config *KvClientConfig) (KvClient, error) {
			return nil, expectedErr
		},
	})
	require.NoError(t, err)

	_, err = pool.GetClient(context.Background())
	require.ErrorIs(t, err, expectedErr)
}

func TestKvClientPoolGetClientIntegration(t *testing.T) {
	testutils.SkipIfShortTest(t)

	auth := &PasswordAuthenticator{
		Username: testutils.TestOpts.Username,
		Password: testutils.TestOpts.Password,
	}
	clientConfig := KvClientConfig{
		Address:        testutils.TestOpts.MemdAddrs[0],
		TlsConfig:      nil,
		SelectedBucket: testutils.TestOpts.BucketName,
		Authenticator:  auth,
	}

	logger, err := zap.NewDevelopment()
	require.NoError(t, err)

	pool, err := NewKvClientPool(&KvClientPoolConfig{
		NumConnections: 5,
		ClientConfig:   clientConfig,
	}, &KvClientPoolOptions{
		Logger: logger,
	})
	require.NoError(t, err)

	// This is done twice to test different codepaths, this is maybe testing implementation detail a bit.
	_, err = pool.GetClient(context.Background())
	require.NoError(t, err)

	_, err = pool.GetClient(context.Background())
	require.NoError(t, err)
}
