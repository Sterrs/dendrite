// Copyright 2017 Vector Creations Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	"github.com/matrix-org/dendrite/clientapi/auth/storage/accounts"
	"github.com/matrix-org/dendrite/clientapi/auth/storage/devices"
	"github.com/matrix-org/dendrite/clientapi/producers"
	"github.com/matrix-org/dendrite/common"
	"github.com/matrix-org/dendrite/common/config"
	"github.com/matrix-org/dendrite/common/keydb"
	"github.com/matrix-org/gomatrixserverlib"

	mediaapi_routing "github.com/matrix-org/dendrite/mediaapi/routing"
	mediaapi_storage "github.com/matrix-org/dendrite/mediaapi/storage"

	roomserver_alias "github.com/matrix-org/dendrite/roomserver/alias"
	roomserver_input "github.com/matrix-org/dendrite/roomserver/input"
	roomserver_query "github.com/matrix-org/dendrite/roomserver/query"
	roomserver_storage "github.com/matrix-org/dendrite/roomserver/storage"

	clientapi_consumers "github.com/matrix-org/dendrite/clientapi/consumers"
	clientapi_routing "github.com/matrix-org/dendrite/clientapi/routing"

	syncapi_consumers "github.com/matrix-org/dendrite/syncapi/consumers"
	syncapi_routing "github.com/matrix-org/dendrite/syncapi/routing"
	syncapi_storage "github.com/matrix-org/dendrite/syncapi/storage"
	syncapi_sync "github.com/matrix-org/dendrite/syncapi/sync"
	syncapi_types "github.com/matrix-org/dendrite/syncapi/types"

	federationapi_routing "github.com/matrix-org/dendrite/federationapi/routing"

	federationsender_consumers "github.com/matrix-org/dendrite/federationsender/consumers"
	"github.com/matrix-org/dendrite/federationsender/queue"
	federationsender_storage "github.com/matrix-org/dendrite/federationsender/storage"

	log "github.com/Sirupsen/logrus"
	sarama "gopkg.in/Shopify/sarama.v1"
)

var (
	logDir        = os.Getenv("LOG_DIR")
	configPath    = flag.String("config", "dendrite.yaml", "The path to the config file. For more information, see the config file in this repository.")
	httpBindAddr  = flag.String("http-bind-address", ":8008", "The HTTP listening port for the server")
	httpsBindAddr = flag.String("https-bind-address", ":8448", "The HTTPS listening port for the server")
	certFile      = flag.String("tls-cert", "", "The PEM formatted X509 certificate to use for TLS")
	keyFile       = flag.String("tls-key", "", "The PEM private key to use for TLS")
)

func main() {
	common.SetupLogging(logDir)

	flag.Parse()

	if *configPath == "" {
		log.Fatal("--config must be supplied")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Invalid config file: %s", err)
	}

	m := newMonolith(cfg)
	m.setupDatabases()
	m.setupFederation()
	m.setupRoomServer()
	m.setupProducers()
	m.setupNotifiers()
	m.setupConsumers()
	m.setupAPIs()

	// Expose the matrix APIs directly rather than putting them under a /api path.
	go func() {
		log.Info("Listening on ", *httpBindAddr)
		log.Fatal(http.ListenAndServe(*httpBindAddr, m.api))
	}()
	// Handle HTTPS if certificate and key are provided
	go func() {
		if *certFile != "" && *keyFile != "" {
			log.Info("Listening on ", *httpsBindAddr)
			log.Fatal(http.ListenAndServeTLS(*httpsBindAddr, *certFile, *keyFile, m.api))
		}
	}()

	// We want to block forever to let the HTTP and HTTPS handler serve the APIs
	select {}
}

// A monolith contains all the dendrite components.
// Some of the setup functions depend on previous setup functions, so they must
// be called in the same order as they are defined in the file.
type monolith struct {
	cfg *config.Dendrite
	api *mux.Router

	roomServerDB       *roomserver_storage.Database
	accountDB          *accounts.Database
	deviceDB           *devices.Database
	keyDB              *keydb.Database
	mediaAPIDB         *mediaapi_storage.Database
	syncAPIDB          *syncapi_storage.SyncServerDatabase
	federationSenderDB *federationsender_storage.Database

	federation *gomatrixserverlib.FederationClient
	keyRing    gomatrixserverlib.KeyRing

	inputAPI *roomserver_input.RoomserverInputAPI
	queryAPI *roomserver_query.RoomserverQueryAPI
	aliasAPI *roomserver_alias.RoomserverAliasAPI

	roomServerProducer *producers.RoomserverProducer
	userUpdateProducer *producers.UserUpdateProducer
	syncProducer       *producers.SyncAPIProducer

	syncAPINotifier *syncapi_sync.Notifier
}

func newMonolith(cfg *config.Dendrite) *monolith {
	return &monolith{cfg: cfg, api: mux.NewRouter()}
}

func (m *monolith) setupDatabases() {
	var err error
	m.roomServerDB, err = roomserver_storage.Open(string(m.cfg.Database.RoomServer))
	if err != nil {
		panic(err)
	}
	m.accountDB, err = accounts.NewDatabase(string(m.cfg.Database.Account), m.cfg.Matrix.ServerName)
	if err != nil {
		log.Panicf("Failed to setup account database(%q): %s", m.cfg.Database.Account, err.Error())
	}
	m.deviceDB, err = devices.NewDatabase(string(m.cfg.Database.Device), m.cfg.Matrix.ServerName)
	if err != nil {
		log.Panicf("Failed to setup device database(%q): %s", m.cfg.Database.Device, err.Error())
	}
	m.keyDB, err = keydb.NewDatabase(string(m.cfg.Database.ServerKey))
	if err != nil {
		log.Panicf("Failed to setup key database(%q): %s", m.cfg.Database.ServerKey, err.Error())
	}
	m.mediaAPIDB, err = mediaapi_storage.Open(string(m.cfg.Database.MediaAPI))
	if err != nil {
		log.Panicf("Failed to setup sync api database(%q): %s", m.cfg.Database.MediaAPI, err.Error())
	}
	m.syncAPIDB, err = syncapi_storage.NewSyncServerDatabase(string(m.cfg.Database.SyncAPI))
	if err != nil {
		log.Panicf("Failed to setup sync api database(%q): %s", m.cfg.Database.SyncAPI, err.Error())
	}
	m.federationSenderDB, err = federationsender_storage.NewDatabase(string(m.cfg.Database.FederationSender))
	if err != nil {
		log.Panicf("startup: failed to create federation sender database with data source %s : %s", m.cfg.Database.FederationSender, err)
	}
}

func (m *monolith) setupFederation() {
	m.federation = gomatrixserverlib.NewFederationClient(
		m.cfg.Matrix.ServerName, m.cfg.Matrix.KeyID, m.cfg.Matrix.PrivateKey,
	)

	m.keyRing = gomatrixserverlib.KeyRing{
		KeyFetchers: []gomatrixserverlib.KeyFetcher{
			// TODO: Use perspective key fetchers for production.
			&gomatrixserverlib.DirectKeyFetcher{Client: m.federation.Client},
		},
		KeyDatabase: m.keyDB,
	}
}

func (m *monolith) setupRoomServer() {
	kafkaProducer, err := sarama.NewSyncProducer(m.cfg.Kafka.Addresses, nil)
	if err != nil {
		panic(err)
	}

	m.inputAPI = &roomserver_input.RoomserverInputAPI{
		DB:                   m.roomServerDB,
		Producer:             kafkaProducer,
		OutputRoomEventTopic: string(m.cfg.Kafka.Topics.OutputRoomEvent),
	}

	m.queryAPI = &roomserver_query.RoomserverQueryAPI{
		DB: m.roomServerDB,
	}

	m.aliasAPI = &roomserver_alias.RoomserverAliasAPI{
		DB:       m.roomServerDB,
		Cfg:      m.cfg,
		InputAPI: m.inputAPI,
		QueryAPI: m.queryAPI,
	}
}

func (m *monolith) setupProducers() {
	var err error
	m.roomServerProducer = producers.NewRoomserverProducer(m.inputAPI)
	m.userUpdateProducer, err = producers.NewUserUpdateProducer(
		m.cfg.Kafka.Addresses, string(m.cfg.Kafka.Topics.UserUpdates),
	)
	if err != nil {
		log.Panicf("Failed to setup kafka producers(%q): %s", m.cfg.Kafka.Addresses, err)
	}
	m.syncProducer, err = producers.NewSyncAPIProducer(
		m.cfg.Kafka.Addresses, string(m.cfg.Kafka.Topics.OutputClientData),
	)
	if err != nil {
		log.Panicf("Failed to setup kafka producers(%q): %s", m.cfg.Kafka.Addresses, err)
	}
}

func (m *monolith) setupNotifiers() {
	pos, err := m.syncAPIDB.SyncStreamPosition()
	if err != nil {
		log.Panicf("startup: failed to get latest sync stream position : %s", err)
	}

	m.syncAPINotifier = syncapi_sync.NewNotifier(syncapi_types.StreamPosition(pos))
	if err = m.syncAPINotifier.Load(m.syncAPIDB); err != nil {
		log.Panicf("startup: failed to set up notifier: %s", err)
	}
}

func (m *monolith) setupConsumers() {
	clientAPIConsumer, err := clientapi_consumers.NewOutputRoomEvent(m.cfg, m.accountDB)
	if err != nil {
		log.Panicf("startup: failed to create room server consumer: %s", err)
	}
	if err = clientAPIConsumer.Start(); err != nil {
		log.Panicf("startup: failed to start room server consumer")
	}

	syncAPIRoomConsumer, err := syncapi_consumers.NewOutputRoomEvent(
		m.cfg, m.syncAPINotifier, m.syncAPIDB,
	)
	if err != nil {
		log.Panicf("startup: failed to create room server consumer: %s", err)
	}
	if err = syncAPIRoomConsumer.Start(); err != nil {
		log.Panicf("startup: failed to start room server consumer: %s", err)
	}

	syncAPIClientConsumer, err := syncapi_consumers.NewOutputClientData(
		m.cfg, m.syncAPINotifier, m.syncAPIDB,
	)
	if err != nil {
		log.Panicf("startup: failed to create client API server consumer: %s", err)
	}
	if err = syncAPIClientConsumer.Start(); err != nil {
		log.Panicf("startup: failed to start client API server consumer: %s", err)
	}

	federationSenderQueues := queue.NewOutgoingQueues(m.cfg.Matrix.ServerName, m.federation)

	federationSenderRoomConsumer, err := federationsender_consumers.NewOutputRoomEvent(
		m.cfg, federationSenderQueues, m.federationSenderDB,
	)
	if err != nil {
		log.WithError(err).Panicf("startup: failed to create room server consumer")
	}
	if err = federationSenderRoomConsumer.Start(); err != nil {
		log.WithError(err).Panicf("startup: failed to start room server consumer")
	}
}

func (m *monolith) setupAPIs() {
	clientapi_routing.Setup(
		m.api, http.DefaultClient, *m.cfg, m.roomServerProducer,
		m.queryAPI, m.aliasAPI, m.accountDB, m.deviceDB, m.federation, m.keyRing,
		m.userUpdateProducer, m.syncProducer,
	)

	mediaapi_routing.Setup(
		m.api, http.DefaultClient, m.cfg, m.mediaAPIDB,
	)

	syncapi_routing.Setup(m.api, syncapi_sync.NewRequestPool(
		m.syncAPIDB, m.syncAPINotifier, m.accountDB,
	), m.deviceDB)

	federationapi_routing.Setup(
		m.api, *m.cfg, m.queryAPI, m.roomServerProducer, m.keyRing, m.federation,
	)
}
