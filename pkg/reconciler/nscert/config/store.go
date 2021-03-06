/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"context"

	network "knative.dev/networking/pkg"
	"knative.dev/pkg/configmap"
	routecfg "knative.dev/serving/pkg/reconciler/route/config"
)

type cfgKey struct{}

// Config of the nscert controller.
type Config struct {
	Network *network.Config
	Domain  *routecfg.Domain
}

// FromContext fetches config from context.
func FromContext(ctx context.Context) *Config {
	return ctx.Value(cfgKey{}).(*Config)
}

// ToContext adds config to given context.
func ToContext(ctx context.Context, c *Config) context.Context {
	return context.WithValue(ctx, cfgKey{}, c)
}

// Store is based on configmap.UntypedStore and is used to store and watch for
// updates to configuration related to routes (currently only config-domain).
type Store struct {
	*configmap.UntypedStore
}

// NewStore creates a configmap.UntypedStore based config store.
//
// logger must be non-nil implementation of configmap.Logger (commonly used
// loggers conform)
//
// onAfterStore is a variadic list of callbacks to run
// after the ConfigMap has been processed and stored.
//
// See also: configmap.NewUntypedStore().
func NewStore(logger configmap.Logger, onAfterStore ...func(name string, value interface{})) *Store {
	store := &Store{
		UntypedStore: configmap.NewUntypedStore(
			"namespace",
			logger,
			configmap.Constructors{
				network.ConfigName:        network.NewConfigFromConfigMap,
				routecfg.DomainConfigName: routecfg.NewDomainFromConfigMap,
			},
			onAfterStore...,
		),
	}

	return store
}

// ToContext adds Store contents to given context.
func (s *Store) ToContext(ctx context.Context) context.Context {
	return ToContext(ctx, s.Load())
}

// Load fetches config from Store.
func (s *Store) Load() *Config {
	return &Config{
		Network: s.UntypedLoad(network.ConfigName).(*network.Config).DeepCopy(),
		Domain:  s.UntypedLoad(routecfg.DomainConfigName).(*routecfg.Domain).DeepCopy(),
	}
}
