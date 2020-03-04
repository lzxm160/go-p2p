package p2p

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWindowsPrivetaAddress(t *testing.T) {
	opts := []Option{
		HostName("0.0.0.0"),
		Port(1234),
	}
	host, err := NewHost(context.Background(), opts...)
	require.NoError(t, err)
	for _,addr:=range host.Addresses(){
		require.False(t,strings.Contains(addr.String(),"169.254."))
	}
}

func TestBroadcast(t *testing.T) {
	runP2P := func(t *testing.T, options ...Option) {
		ctx := context.Background()
		n := 10
		hosts := make([]*Host, n)
		for i := 0; i < n; i++ {
			opts := []Option{
				Port(30000 + i),
				SecureIO(),
				MasterKey(strconv.Itoa(i)),
			}
			opts = append(opts, options...)
			host, err := NewHost(ctx, opts...)
			require.NoError(t, err)
			require.NoError(t, host.AddBroadcastPubSub("test", func(ctx context.Context, data []byte) error {
				fmt.Print(string(data))
				fmt.Printf(", received by %s\n", host.HostIdentity())
				return nil
			}))
			hosts[i] = host
		}

		bootstrapInfo := hosts[0].Info()
		for i := 0; i < n; i++ {
			if i != 0 {
				require.NoError(t, hosts[i].Connect(ctx, bootstrapInfo))
			}
			hosts[i].JoinOverlay(ctx)
		}

		for i := 0; i < n; i++ {
			require.NoError(
				t,
				hosts[i].Broadcast("test", []byte(fmt.Sprintf("msg sent from %s", hosts[i].HostIdentity()))),
			)
		}

		for i := 0; i < n; i++ {
			require.NoError(t, hosts[i].Close())
		}
	}

	t.Run("flood", func(t *testing.T) {
		runP2P(t)
	})

	t.Run("gossip", func(t *testing.T) {
		runP2P(t, Gossip())
	})
}

func TestUnicast(t *testing.T) {
	ctx := context.Background()
	n := 10
	hosts := make([]*Host, n)
	for i := 0; i < n; i++ {
		host, err := NewHost(ctx, Port(30000+i), SecureIO(), MasterKey(strconv.Itoa(i)))
		require.NoError(t, err)
		require.NoError(t, host.AddUnicastPubSub("test", func(ctx context.Context, w io.Writer, data []byte) error {
			fmt.Print(string(data))
			fmt.Printf(", received by %s\n", host.HostIdentity())
			return nil
		}))
		hosts[i] = host
	}

	bootstrapInfo := hosts[0].Info()
	for i := 0; i < n; i++ {
		if i != 0 {
			require.NoError(t, hosts[i].Connect(ctx, bootstrapInfo))
		}
		hosts[i].JoinOverlay(ctx)
	}

	for i, host := range hosts {
		neighbors, err := host.Neighbors(ctx)
		require.NoError(t, err)
		require.True(t, len(neighbors) > 0)

		for _, neighbor := range neighbors {
			require.NoError(
				t,
				host.Unicast(ctx, neighbor, "test", []byte(fmt.Sprintf("msg sent from %s", hosts[i].HostIdentity()))),
			)
		}
	}

	for i := 0; i < n; i++ {
		require.NoError(t, hosts[i].Close())
	}
}
