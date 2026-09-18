package enrollment

import "testing"

func FuzzBootstrapEnvelopeNoPanic(f *testing.F) {
	recipient, _, envelope := testBootstrapEnvelopeForFuzz(f)
	defer recipient.Destroy()
	f.Add(envelope)
	f.Add([]byte{})
	f.Add([]byte(`{"version":1}`))
	f.Fuzz(func(t *testing.T, wire []byte) {
		opened, err := recipient.OpenBootstrap(testBootstrapBinding(), wire)
		if err != nil && err != ErrBootstrapDenied {
			t.Fatal("fuzz input returned a non-generic error")
		}
		opened.Destroy()
	})
}

func FuzzBootstrapRecipientPublicKeyNoPanic(f *testing.F) {
	recipient, _, _ := testBootstrapEnvelopeForFuzz(f)
	defer recipient.Destroy()
	key, err := recipient.PublicKey()
	if err != nil {
		f.Fatalf("PublicKey: %v", err)
	}
	wire, err := key.Wire()
	if err != nil {
		f.Fatalf("Wire: %v", err)
	}
	f.Add(wire)
	f.Add([]byte{})
	f.Add([]byte(`{"version":1,"publicKey":"!"}`))
	f.Fuzz(func(t *testing.T, wire []byte) {
		if _, err := ParseBootstrapRecipientPublicKey(wire); err != nil && err != ErrBootstrapDenied {
			t.Fatal("fuzz public-key input returned a non-generic error")
		}
	})
}

func testBootstrapEnvelopeForFuzz(t testing.TB) (BootstrapRecipient, BootstrapBinding, []byte) {
	t.Helper()
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("NewBootstrapRecipient: %v", err)
	}
	publicKey, err := recipient.PublicKey()
	if err != nil {
		recipient.Destroy()
		t.Fatalf("PublicKey: %v", err)
	}
	binding := testBootstrapBinding()
	grant := testBootstrapGrant(t, binding)
	envelope, err := SealBootstrap(publicKey, binding, grant)
	grant.Destroy()
	if err != nil {
		recipient.Destroy()
		t.Fatalf("SealBootstrap: %v", err)
	}
	return recipient, testBootstrapBinding(), envelope
}
