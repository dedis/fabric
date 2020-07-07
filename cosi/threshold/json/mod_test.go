package json

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.dedis.ch/dela/cosi/threshold"
	"go.dedis.ch/dela/internal/testing/fake"
	"go.dedis.ch/dela/serde"
	"go.dedis.ch/dela/serde/json"
)

func TestFormat_Encode(t *testing.T) {
	format := sigFormat{}
	sig := threshold.NewSignature(fake.Signature{}, []byte{0xab})
	ctx := json.NewContext()

	data, err := format.Encode(ctx, sig)
	require.NoError(t, err)
	require.Equal(t, `{"Mask":"qw==","Aggregate":{}}`, string(data))

	_, err = format.Encode(ctx, fake.Message{})
	require.EqualError(t, err, "unsupported message of type 'fake.Message'")

	_, err = format.Encode(fake.NewBadContext(), sig)
	require.EqualError(t, err, "couldn't marshal: fake error")

	sig = threshold.NewSignature(fake.NewBadSignature(), nil)
	_, err = format.Encode(ctx, sig)
	require.EqualError(t, err, "couldn't serialize aggregate: fake error")
}

func TestFormat_Decode(t *testing.T) {
	format := sigFormat{}
	ctx := serde.WithFactory(json.NewContext(), threshold.AggKey{}, fake.SignatureFactory{})

	sig, err := format.Decode(ctx, []byte(`{"Mask":[1],"Aggregate":{}}`))
	require.NoError(t, err)
	require.Equal(t, []byte{1}, sig.(*threshold.Signature).GetMask())

	_, err = format.Decode(fake.NewBadContext(), []byte(`{}`))
	require.EqualError(t, err, "couldn't unmarshal message: fake error")

	ctx = serde.WithFactory(ctx, threshold.AggKey{}, fake.NewBadSignatureFactory())
	_, err = format.Decode(ctx, []byte(`{}`))
	require.EqualError(t, err, "couldn't deserialize signature: fake error")

	ctx = serde.WithFactory(ctx, threshold.AggKey{}, nil)
	_, err = format.Decode(ctx, []byte(`{}`))
	require.EqualError(t, err, "invalid factory of type '<nil>'")
}