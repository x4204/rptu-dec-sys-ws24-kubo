package libp2p

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"time"

	offroute "github.com/ipfs/boxo/routing/offline"
	ds "github.com/ipfs/go-datastore"
	ddht "github.com/libp2p/go-libp2p-kad-dht/dual"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	namesys "github.com/libp2p/go-libp2p-pubsub-router"
	record "github.com/libp2p/go-libp2p-record"
	routinghelpers "github.com/libp2p/go-libp2p-routing-helpers"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	"go.uber.org/fx"

	config "github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core/node/helpers"
	"github.com/ipfs/kubo/repo"
	irouting "github.com/ipfs/kubo/routing"
)

type Router struct {
	routing.Routing

	Priority int // less = more important
}

type p2pRouterOut struct {
	fx.Out

	Router Router `group:"routers"`
}

type processInitialRoutingIn struct {
	fx.In

	Router routing.Routing `name:"initialrouting"`

	// For setting up experimental DHT client
	Host      host.Host
	Repo      repo.Repo
	Validator record.Validator
}

type processInitialRoutingOut struct {
	fx.Out

	Router        Router                 `group:"routers"`
	ContentRouter routing.ContentRouting `group:"content-routers"`

	DHT       *ddht.DHT
	DHTClient routing.Routing `name:"dhtc"`
}

type AddrInfoChan chan peer.AddrInfo

func BaseRouting(cfg *config.Config) interface{} {
	return func(lc fx.Lifecycle, in processInitialRoutingIn) (out processInitialRoutingOut, err error) {
		var dualDHT *ddht.DHT
		if dht, ok := in.Router.(*ddht.DHT); ok {
			dualDHT = dht

			lc.Append(fx.Hook{
				OnStop: func(ctx context.Context) error {
					return dualDHT.Close()
				},
			})
		}

		if cr, ok := in.Router.(routinghelpers.ComposableRouter); ok {
			for _, r := range cr.Routers() {
				if dht, ok := r.(*ddht.DHT); ok {
					dualDHT = dht
					lc.Append(fx.Hook{
						OnStop: func(ctx context.Context) error {
							return dualDHT.Close()
						},
					})
					break
				}
			}
		}

		return processInitialRoutingOut{
			Router: Router{
				Priority: 1000,
				Routing:  in.Router,
			},
			DHT:           dualDHT,
			DHTClient:     dualDHT,
			ContentRouter: in.Router,
		}, nil
	}
}

type p2pOnlineContentRoutingIn struct {
	fx.In

	ContentRouter []routing.ContentRouting `group:"content-routers"`
}

// ContentRouting will get all routers that can do contentRouting and add them
// all together using a TieredRouter. It will be used for topic discovery.
func ContentRouting(in p2pOnlineContentRoutingIn) routing.ContentRouting {
	var routers []routing.Routing

	return routinghelpers.Tiered{
		Routers: routers,
	}
}

type p2pOnlineRoutingIn struct {
	fx.In

	Routers   []Router `group:"routers"`
	Validator record.Validator
}

// Routing will get all routers obtained from different methods
// (delegated routers, pub-sub, and so on) and add them all together
// using a TieredRouter.
func Routing(in p2pOnlineRoutingIn) irouting.ProvideManyRouter {
	routers := in.Routers

	sort.SliceStable(routers, func(i, j int) bool {
		return routers[i].Priority < routers[j].Priority
	})

	var cRouters []*routinghelpers.ParallelRouter

	return routinghelpers.NewComposableParallel(cRouters)
}

// OfflineRouting provides a special Router to the routers list when we are creating a offline node.
func OfflineRouting(dstore ds.Datastore, validator record.Validator) p2pRouterOut {
	return p2pRouterOut{
		Router: Router{
			Routing:  offroute.NewOfflineRouter(dstore, validator),
			Priority: 10000,
		},
	}
}

type p2pPSRoutingIn struct {
	fx.In

	Validator record.Validator
	Host      host.Host
	PubSub    *pubsub.PubSub `optional:"true"`
}

func PubsubRouter(mctx helpers.MetricsCtx, lc fx.Lifecycle, in p2pPSRoutingIn) (p2pRouterOut, *namesys.PubsubValueStore, error) {
	psRouter, err := namesys.NewPubsubValueStore(
		helpers.LifecycleCtx(mctx, lc),
		in.Host,
		in.PubSub,
		in.Validator,
		namesys.WithRebroadcastInterval(time.Minute),
	)
	if err != nil {
		return p2pRouterOut{}, nil, err
	}

	return p2pRouterOut{
		Router: Router{
			Routing: &routinghelpers.Compose{
				ValueStore: &routinghelpers.LimitedValueStore{
					ValueStore: psRouter,
					Namespaces: []string{"ipns"},
				},
			},
			Priority: 100,
		},
	}, psRouter, nil
}

func autoRelayFeeder(cfgPeering config.Peering, peerChan chan<- peer.AddrInfo) fx.Option {
	return fx.Invoke(func(lc fx.Lifecycle, h host.Host, dht *ddht.DHT) {
		_, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})

		defer func() {
			if r := recover(); r != nil {
				fmt.Println("Recovering from unexpected error in AutoRelayFeeder:", r)
				debug.PrintStack()
			}
		}()

		lc.Append(fx.Hook{
			OnStop: func(_ context.Context) error {
				cancel()
				<-done
				return nil
			},
		})
	})
}
