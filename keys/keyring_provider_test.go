package keys

import (
	"encoding/hex"
	"testing"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// testAppHex is a real secp256k1 private key (localnet app1), used only as a
// well-formed hex input to import into a transient keyring.
const testAppHex = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a"

// newInMemoryKeyring returns a transient keyring for tests.
func newInMemoryKeyring(t *testing.T) keyring.Keyring {
	t.Helper()
	registry := codectypes.NewInterfaceRegistry()
	cryptocodec.RegisterInterfaces(registry)
	cdc := codec.NewProtoCodec(registry)
	return keyring.NewInMemory(cdc)
}

// TestKeyringProvider_LoadKeyByName proves a named key imported into the keyring
// is returned as the exact secp256k1 private key (round-trips to the same hex)
// with a non-empty operator address — this is what lets the relay CLI resolve
// --app-key/--gateway-key without ever putting hex on the command line.
func TestKeyringProvider_LoadKeyByName(t *testing.T) {
	kr := newInMemoryKeyring(t)
	require.NoError(t, kr.ImportPrivKeyHex("app", testAppHex, "secp256k1"))

	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	p := NewKeyringProviderWithKeyring(logger, kr, nil)

	privKey, addr, err := p.LoadKeyByName("app")

	require.NoError(t, err)
	require.NotEmpty(t, addr, "operator address must be derived")
	require.Equal(t, testAppHex, hex.EncodeToString(privKey.Bytes()),
		"the returned private key must round-trip to the imported hex")
}

// TestKeyringProvider_LoadKeyByName_Missing proves an unknown key name is a clear
// error rather than a nil key.
func TestKeyringProvider_LoadKeyByName_Missing(t *testing.T) {
	kr := newInMemoryKeyring(t)
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	p := NewKeyringProviderWithKeyring(logger, kr, nil)

	_, _, err := p.LoadKeyByName("does-not-exist")

	require.Error(t, err)
}
