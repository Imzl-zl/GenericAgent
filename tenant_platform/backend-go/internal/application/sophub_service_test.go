package application

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Imzl-zl/GenericAgent/tenant_platform/backend-go/internal/domain"
)

type fakeSophubClient struct {
	identity domain.SophubIdentity
	remote   domain.SophubRemoteSOP
	keySeen  string
	err      error
}

func (client *fakeSophubClient) Verify(_ context.Context, key string) (domain.SophubIdentity, error) {
	client.keySeen = key
	return client.identity, client.err
}
func (client *fakeSophubClient) Search(context.Context, string, string, int, int) (domain.SophubSearchResult, error) {
	return domain.SophubSearchResult{}, client.err
}
func (client *fakeSophubClient) GetSOP(_ context.Context, key, _ string) (domain.SophubRemoteSOP, error) {
	client.keySeen = key
	return client.remote, client.err
}

type fakeSophubStore struct {
	binding domain.SophubBinding
}

func (store *fakeSophubStore) UpsertSophubBinding(_ context.Context, binding domain.SophubBinding) (domain.SophubBinding, error) {
	store.binding = binding
	return binding, nil
}
func (store *fakeSophubStore) GetSophubBinding(context.Context) (domain.SophubBinding, error) {
	if len(store.binding.APIKeyCiphertext) == 0 {
		return domain.SophubBinding{}, errors.New("binding not found")
	}
	return store.binding, nil
}
type fakeSophubCipher struct {
	plain []byte
}

func (cipher *fakeSophubCipher) Encrypt(plain []byte) ([]byte, int, error) {
	cipher.plain = append([]byte(nil), plain...)
	return []byte("ciphertext-only"), 7, nil
}
func (cipher *fakeSophubCipher) Decrypt(ciphertext []byte, version int) ([]byte, error) {
	if !bytes.Equal(ciphertext, []byte("ciphertext-only")) || version != 7 {
		return nil, errors.New("unexpected ciphertext")
	}
	return append([]byte(nil), cipher.plain...), nil
}

func TestSophubServiceBindsEncryptedWriteOnlyKey(t *testing.T) {
	const key = "sophub-secret-sentinel"
	client := &fakeSophubClient{identity: domain.SophubIdentity{AuthorType: "agent", AgentUID: "agent-1", DisplayName: "platform"}}
	store := &fakeSophubStore{}
	cipher := &fakeSophubCipher{}
	service, err := NewSophubService(SophubServiceConfig{Store: store, Client: client, Cipher: cipher})
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Bind(context.Background(), key, 42)
	if err != nil {
		t.Fatal(err)
	}
	if client.keySeen != key || string(cipher.plain) != key || bytes.Equal(store.binding.APIKeyCiphertext, []byte(key)) {
		t.Fatalf("key was not verified/encrypted correctly")
	}
	if !status.Configured || status.AgentUID != "agent-1" || strings.Contains(status.DisplayName, key) {
		t.Fatalf("status=%+v", status)
	}
}

func TestSophubServiceDoesNotPersistFailedOrLeakKey(t *testing.T) {
	const key = "sophub-secret-sentinel"
	client := &fakeSophubClient{err: errors.New("upstream rejected sophub-secret-sentinel")}
	store := &fakeSophubStore{}
	service, err := NewSophubService(SophubServiceConfig{Store: store, Client: client, Cipher: &fakeSophubCipher{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Bind(context.Background(), key, 42); err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("secret-safe bind err=%v", err)
	}
	if len(store.binding.APIKeyCiphertext) != 0 {
		t.Fatal("failed binding was persisted")
	}
}


func TestSophubServiceFetchRemoteSOPForWorkerProxy(t *testing.T) {
	store := &fakeSophubStore{}
	cipher := &fakeSophubCipher{}
	client := &fakeSophubClient{remote: domain.SophubRemoteSOP{
		ID: "sop-remote", FileType: "markdown", Status: "approved", Content: "# SOP",
	}}
	service, err := NewSophubService(SophubServiceConfig{Store: store, Client: client, Cipher: cipher})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Bind(context.Background(), "admin-key", 1); err != nil {
		t.Fatal(err)
	}
	remote, err := service.FetchRemoteSOP(context.Background(), "sop-remote")
	if err != nil {
		t.Fatal(err)
	}
	if remote.ID != "sop-remote" || remote.Content != "# SOP" {
		t.Fatalf("remote=%+v", remote)
	}
	// 未绑定: 拒绝。
	store2 := &fakeSophubStore{}
	service2, err := NewSophubService(SophubServiceConfig{Store: store2, Client: client, Cipher: cipher})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service2.FetchRemoteSOP(context.Background(), "sop-remote"); err == nil {
		t.Fatal("fetch without binding must fail")
	}
}
