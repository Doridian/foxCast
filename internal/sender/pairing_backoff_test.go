package sender

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExchangePairSetupM1RetriesBackoff(t *testing.T) {
	backoff := tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvState, Value: []byte{2}},
		{Tag: tlvError, Value: []byte{pairingErrorBackoff}},
		{Tag: tlvRetryDelay, Value: []byte{0}},
	})
	wantPublic := []byte{1, 2, 3}
	success := tlv8EncodeOrdered([]tlv8Item{
		{Tag: tlvState, Value: []byte{2}},
		{Tag: tlvPublicKey, Value: wantPublic},
	})
	calls := 0
	m2, err := exchangePairSetupM1(context.Background(), func() ([]byte, error) {
		calls++
		if calls == 1 {
			return backoff, nil
		}
		return success, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("M1 calls = %d, want 2", calls)
	}
	if got := m2[tlvPublicKey]; string(got) != string(wantPublic) {
		t.Fatalf("M2 public key = %x, want %x", got, wantPublic)
	}
}

func TestExchangePairSetupM1BackoffHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := exchangePairSetupM1(ctx, func() ([]byte, error) {
		return tlv8EncodeOrdered([]tlv8Item{
			{Tag: tlvError, Value: []byte{pairingErrorBackoff}},
			{Tag: tlvRetryDelay, Value: []byte{1}},
		}), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestExchangePairSetupM1RejectsInvalidBackoff(t *testing.T) {
	for _, retryDelay := range [][]byte{nil, make([]byte, 9), {31}} {
		_, err := exchangePairSetupM1(context.Background(), func() ([]byte, error) {
			return tlv8EncodeOrdered([]tlv8Item{
				{Tag: tlvError, Value: []byte{pairingErrorBackoff}},
				{Tag: tlvRetryDelay, Value: retryDelay},
			}), nil
		})
		if err == nil || !strings.Contains(err.Error(), "RetryDelay") {
			t.Errorf("RetryDelay %x error = %v", retryDelay, err)
		}
	}
}

func TestExchangePairSetupM1BoundsRetries(t *testing.T) {
	calls := 0
	_, err := exchangePairSetupM1(context.Background(), func() ([]byte, error) {
		calls++
		return tlv8EncodeOrdered([]tlv8Item{
			{Tag: tlvError, Value: []byte{pairingErrorBackoff}},
			{Tag: tlvRetryDelay, Value: []byte{0}},
		}), nil
	})
	if err == nil || !strings.Contains(err.Error(), "persisted") {
		t.Fatalf("error = %v, want retry-bound error", err)
	}
	if calls != pairSetupBackoffRetries+1 {
		t.Fatalf("M1 calls = %d, want %d", calls, pairSetupBackoffRetries+1)
	}
}

func TestPairingRetryDelayLittleEndian(t *testing.T) {
	delay, err := pairingRetryDelay([]byte{2, 0})
	if err != nil {
		t.Fatal(err)
	}
	if delay != 2*time.Second {
		t.Fatalf("delay = %v, want 2s", delay)
	}
}

func TestExchangePairSetupM1NamesOtherErrors(t *testing.T) {
	_, err := exchangePairSetupM1(context.Background(), func() ([]byte, error) {
		return tlv8EncodeOrdered([]tlv8Item{{Tag: tlvError, Value: []byte{2}}}), nil
	})
	if err == nil || !strings.Contains(err.Error(), "authentication (2)") {
		t.Fatalf("error = %v, want named authentication error", err)
	}
}
