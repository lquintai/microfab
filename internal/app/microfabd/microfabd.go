/*
 * SPDX-License-Identifier: Apache-2.0
 */

package microfabd

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/IBM-Blockchain/microfab/internal/pkg/blocks"
	"github.com/IBM-Blockchain/microfab/internal/pkg/ca"
	"github.com/IBM-Blockchain/microfab/internal/pkg/channel"
	"github.com/IBM-Blockchain/microfab/internal/pkg/console"
	"github.com/IBM-Blockchain/microfab/internal/pkg/couchdb"
	"github.com/IBM-Blockchain/microfab/internal/pkg/identity"
	"github.com/IBM-Blockchain/microfab/internal/pkg/orderer"
	"github.com/IBM-Blockchain/microfab/internal/pkg/organization"
	"github.com/IBM-Blockchain/microfab/internal/pkg/peer"
	"github.com/IBM-Blockchain/microfab/internal/pkg/proxy"
	"github.com/IBM-Blockchain/microfab/pkg/client"
	"github.com/hyperledger/fabric-protos-go/common"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

var logger = log.New(os.Stdout, fmt.Sprintf("[%16s] ", "microfabd"), log.LstdFlags)

const startPort = 2000
const endPort = 3000

// Microfab represents an instance of the Microfab application.
type Microfab struct {
	sync.Mutex
	sigs                   chan os.Signal
	done                   chan struct{}
	started                bool
	config                 *Config
	state                  *State
	ordererOrganization    *organization.Organization
	endorsingOrganizations []*organization.Organization
	organizations          []*organization.Organization
	orderer                *orderer.Orderer
	ordererConnection      *orderer.Connection
	couchDB                *couchdb.CouchDB
	couchDBProxies         []*couchdb.Proxy
	peers                  []*peer.Peer
	peerConnections        []*peer.Connection
	cas                    []*ca.CA
	genesisBlocks          map[string]*common.Block
	console                *console.Console
	proxy                  *proxy.Proxy
	currentPort            int
}

// State represents the state that should be persisted between instances.
type State struct {
	Hash []byte                      `json:"hash"`
	CAS  map[string]*client.Identity `json:"cas"`
}

// New creates an instance of the Microfab application.
func New() (*Microfab, error) {
	config, err := DefaultConfig()
	if err != nil {
		return nil, err
	}
	return &Microfab{
		config:      config,
		sigs:        make(chan os.Signal, 1),
		done:        make(chan struct{}, 1),
		started:     false,
		currentPort: startPort,
	}, nil
}

// Start starts the Microfab application.
func (m *Microfab) Start() error {

	// Grab the start time and say hello.
	startTime := time.Now()
	logger.Print("Starting Microfab ...")

	// Ensure anything we start is stopped.
	defer func() {
		if !m.started {
			m.stop()
		}
	}()

	// Calculate the config hash.
	config, err := json.Marshal(m.config)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(config)

	// See if the state exists.
	if m.stateExists() {
		if temp, err := m.loadState(); err != nil {
			logger.Printf("Could not load state: %v\n", err)
		} else if bytes.Equal(hash[:], temp.Hash) {
			logger.Println("Loaded state")
			m.state = temp
		} else {
			logger.Println("Config has changed, loaded state is invalid")
		}
	}

	// Ensure the directory exists and is empty.
	if m.state == nil {
		err = m.ensureDirectory()
		if err != nil {
			return err
		}
	}

	// Create all of the organizations.
	ctx := context.Background()
	eg, _ := errgroup.WithContext(ctx)
	eg.Go(func() error {
		return m.createOrderingOrganization(m.config.OrderingOrganization)
	})
	for i := range m.config.EndorsingOrganizations {
		organization := m.config.EndorsingOrganizations[i]
		eg.Go(func() error {
			return m.createEndorsingOrganization(organization)
		})
	}
	err = eg.Wait()
	if err != nil {
		return err
	}

	// Sort the list of organizations by name, and then join all the organizations together.
	sort.Slice(m.endorsingOrganizations, func(i, j int) bool {
		return m.endorsingOrganizations[i].Name() < m.endorsingOrganizations[j].Name()
	})
	m.organizations = append(m.organizations, m.ordererOrganization)
	m.organizations = append(m.organizations, m.endorsingOrganizations...)

	// Wait for CouchDB to start.
	if m.config.CouchDB {
		err = m.waitForCouchDB()
		if err != nil {
			return err
		}
	}

	// Create and start all of the components (orderer, peers, CAs).
	eg.Go(func() error {
		apiPort := m.allocatePort()
		operationsPort := m.allocatePort()
		return m.createAndStartOrderer(m.ordererOrganization, apiPort, operationsPort)
	})
	for i := range m.endorsingOrganizations {
		organization := m.endorsingOrganizations[i]
		eg.Go(func() error {
			peerAPIPort := m.allocatePort()
			peerChaincodePort := m.allocatePort()
			peerOperationsPort := m.allocatePort()
			if m.config.CouchDB {
				couchDBProxyPort := m.allocatePort()
				go m.createAndStartCouchDBProxy(organization, couchDBProxyPort)
				return m.createAndStartPeer(organization, peerAPIPort, peerChaincodePort, peerOperationsPort, m.config.CouchDB, couchDBProxyPort)
			}
			return m.createAndStartPeer(organization, peerAPIPort, peerChaincodePort, peerOperationsPort, false, 0)
		})
		if m.config.CertificateAuthorities {
			eg.Go(func() error {
				caAPIPort := m.allocatePort()
				caOperationsPort := m.allocatePort()
				return m.createAndStartCA(organization, caAPIPort, caOperationsPort)
			})
		}
	}
	err = eg.Wait()
	if err != nil {
		return err
	}

	// Sort the list of peers by their organization name.
	sort.Slice(m.peers, func(i, j int) bool {
		return m.peers[i].Organization().Name() < m.peers[j].Organization().Name()
	})

	// Create and start the console.
	consolePort := m.allocatePort()
	console, err := console.New(m.organizations, m.orderer, m.peers, m.cas, consolePort, fmt.Sprintf("http://console.%s:%d", m.config.Domain, m.config.Port))
	if err != nil {
		return err
	}
	m.console = console
	go console.Start()

	// Create and start the proxy.
	proxy, err := proxy.New(console, m.orderer, m.peers, m.cas, m.config.Port)
	if err != nil {
		return err
	}
	m.proxy = proxy
	go proxy.Start()

	// Connect to all of the components.
	channelCreator := m.endorsingOrganizations[0]
	ordererConnection, err := orderer.Connect(m.orderer, channelCreator.MSPID(), channelCreator.Admin())
	if err != nil {
		return err
	}
	m.ordererConnection = ordererConnection
	defer m.ordererConnection.Close()
	for _, p := range m.peers {
		peerConnection, err := peer.Connect(p, p.Organization().MSPID(), p.Organization().Admin())
		if err != nil {
			return err
		}
		m.peerConnections = append(m.peerConnections, peerConnection)
	}
	defer func() {
		for _, peerConnection := range m.peerConnections {
			peerConnection.Close()
		}
	}()

	// Create and join all of the channels.
	if m.state == nil {
		for i := range m.config.Channels {
			channel := m.config.Channels[i]
			eg.Go(func() error {
				return m.createAndJoinChannel(channel)
			})
		}
		err = eg.Wait()
		if err != nil {
			return err
		}
	}

	// Write the state for next time.
	err = m.saveState()
	if err != nil {
		return err
	}

	// Say how long start up took, then wait for signals.
	readyTime := time.Now()
	startupDuration := readyTime.Sub(startTime)
	logger.Printf("Microfab started in %vms", startupDuration.Milliseconds())
	signal.Notify(m.sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-m.sigs
		logger.Printf("Stopping Microfab due to signal ...")
		m.stop()
		logger.Printf("Microfab stopped")
		close(m.done)
		m.started = false
	}()
	m.started = true
	return nil

}

// Stop stops the Microfab application.
func (m *Microfab) Stop() {
	if m.started {
		m.sigs <- syscall.SIGTERM
		<-m.done
	}
}

// Wait waits for the Microfab application.
func (m *Microfab) Wait() {
	if m.started {
		<-m.done
	}
}

func (m *Microfab) allocatePort() int {
	m.Lock()
	defer m.Unlock()
	if m.currentPort >= endPort {
		logger.Fatalf("Failed to allocate port, port range %d-%d exceeded", startPort, endPort)
	}
	result := m.currentPort
	m.currentPort++
	return result
}

func (m *Microfab) ensureDirectory() error {
	if m.directoryExists() {
		err := m.removeDirectory()
		if err != nil {
			return err
		}
	} else {
		err := m.createDirectory()
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Microfab) directoryExists() bool {
	if _, err := os.Stat(m.config.Directory); os.IsNotExist(err) {
		return false
	}
	return true
}

func (m *Microfab) createDirectory() error {
	return os.MkdirAll(m.config.Directory, 0755)
}

func (m *Microfab) removeDirectory() error {
	file, err := os.Open(m.config.Directory)
	if err != nil {
		return err
	}
	defer file.Close()
	names, err := file.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		err = os.RemoveAll(path.Join(m.config.Directory, name))
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *Microfab) stateExists() bool {
	statePath := path.Join(m.config.Directory, "state.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		return false
	}
	return true
}

func (m *Microfab) loadState() (*State, error) {
	statePath := path.Join(m.config.Directory, "state.json")
	file, err := os.Open(statePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	state := &State{}
	err = json.NewDecoder(file).Decode(&state)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (m *Microfab) saveState() error {
	statePath := path.Join(m.config.Directory, "state.json")
	file, err := os.OpenFile(statePath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	config, err := json.Marshal(m.config)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(config)
	state := &State{
		Hash: hash[:],
		CAS:  map[string]*client.Identity{},
	}
	state.CAS[m.ordererOrganization.Name()] = m.ordererOrganization.CA().ToClient()
	for _, endorsingOrganization := range m.endorsingOrganizations {
		state.CAS[endorsingOrganization.Name()] = endorsingOrganization.CA().ToClient()
	}
	return json.NewEncoder(file).Encode(&state)
}

func (m *Microfab) createOrderingOrganization(config Organization) error {
	logger.Printf("Creating ordering organization %s ...", config.Name)
	var ca *identity.Identity
	if m.state != nil {
		temp, ok := m.state.CAS[config.Name]
		if ok {
			var err error
			ca, err = identity.FromClient(temp)
			if err != nil {
				return err
			}
		}
	}
	organization, err := organization.New(config.Name, ca)
	if err != nil {
		return err
	}
	m.Lock()
	m.ordererOrganization = organization
	m.Unlock()
	logger.Printf("Created ordering organization %s", config.Name)
	return nil
}

func (m *Microfab) createEndorsingOrganization(config Organization) error {
	logger.Printf("Creating endorsing organization %s ...", config.Name)
	var ca *identity.Identity
	if m.state != nil {
		temp, ok := m.state.CAS[config.Name]
		if ok {
			var err error
			ca, err = identity.FromClient(temp)
			if err != nil {
				return err
			}
		}
	}
	organization, err := organization.New(config.Name, ca)
	if err != nil {
		return err
	}
	m.Lock()
	m.endorsingOrganizations = append(m.endorsingOrganizations, organization)
	m.Unlock()
	logger.Printf("Created endorsing organization %s", config.Name)
	return nil
}

func (m *Microfab) createAndStartOrderer(organization *organization.Organization, apiPort, operationsPort int) error {
	logger.Printf("Creating and starting orderer for ordering organization %s ...", organization.Name())
	directory := path.Join(m.config.Directory, "orderer")
	orderer, err := orderer.New(
		organization,
		directory,
		int32(apiPort),
		fmt.Sprintf("grpc://orderer-api.%s:%d", m.config.Domain, m.config.Port),
		int32(operationsPort),
		fmt.Sprintf("http://orderer-operations.%s:%d", m.config.Domain, m.config.Port),
	)
	if err != nil {
		return err
	}
	m.Lock()
	m.orderer = orderer
	m.Unlock()
	err = orderer.Start(m.endorsingOrganizations, m.config.Timeout)
	if err != nil {
		return err
	}
	logger.Printf("Created and started orderer for ordering organization %s", organization.Name())
	return nil
}

func (m *Microfab) waitForCouchDB() error {
	logger.Printf("Waiting for CouchDB to start ...")
	couchDB, err := couchdb.New("http://localhost:5984")
	if err != nil {
		return err
	}
	m.Lock()
	m.couchDB = couchDB
	m.Unlock()
	err = couchDB.WaitFor(m.config.Timeout)
	if err != nil {
		return err
	}
	logger.Printf("CouchDB has started")
	return nil
}

func (m *Microfab) createAndStartCouchDBProxy(organization *organization.Organization, port int) error {
	logger.Printf("Creating and starting CouchDB proxy for endorsing organization %s ...", organization.Name())
	prefix := strings.ToLower(organization.Name())
	proxy, err := m.couchDB.NewProxy(prefix, port)
	if err != nil {
		return err
	}
	m.Lock()
	m.couchDBProxies = append(m.couchDBProxies, proxy)
	m.Unlock()
	err = proxy.Start()
	if err != nil {
		return err
	}
	logger.Printf("Created and started CouchDB proxy for endorsing organization %s", organization.Name())
	return nil
}

func (m *Microfab) createAndStartPeer(organization *organization.Organization, apiPort, chaincodePort, operationsPort int, couchDB bool, couchDBProxyPort int) error {
	logger.Printf("Creating and starting peer for endorsing organization %s ...", organization.Name())
	organizationName := organization.Name()
	lowerOrganizationName := strings.ToLower(organizationName)
	peerDirectory := path.Join(m.config.Directory, fmt.Sprintf("peer-%s", lowerOrganizationName))
	peer, err := peer.New(
		organization,
		peerDirectory,
		int32(apiPort),
		fmt.Sprintf("grpc://%speer-api.%s:%d", lowerOrganizationName, m.config.Domain, m.config.Port),
		int32(chaincodePort),
		fmt.Sprintf("grpc://%speer-chaincode.%s:%d", lowerOrganizationName, m.config.Domain, m.config.Port),
		int32(operationsPort),
		fmt.Sprintf("http://%speer-operations.%s:%d", lowerOrganizationName, m.config.Domain, m.config.Port),
		couchDB,
		int32(couchDBProxyPort),
	)
	if err != nil {
		return err
	}
	m.Lock()
	m.peers = append(m.peers, peer)
	m.Unlock()
	err = peer.Start(m.config.Timeout)
	if err != nil {
		return err
	}
	logger.Printf("Created and started peer for endorsing organization %s", organization.Name())
	return nil
}

func (m *Microfab) createAndStartCA(organization *organization.Organization, apiPort, operationsPort int) error {
	logger.Printf("Creating and starting CA for endorsing organization %s ...", organization.Name())
	organizationName := organization.Name()
	lowerOrganizationName := strings.ToLower(organizationName)
	caDirectory := path.Join(m.config.Directory, fmt.Sprintf("ca-%s", lowerOrganizationName))
	theCA, err := ca.New(
		organization,
		caDirectory,
		int32(apiPort),
		fmt.Sprintf("http://%sca-api.%s:%d", lowerOrganizationName, m.config.Domain, m.config.Port),
		int32(operationsPort),
		fmt.Sprintf("http://%sca-operations.%s:%d", lowerOrganizationName, m.config.Domain, m.config.Port),
	)
	if err != nil {
		return err
	}
	m.Lock()
	m.cas = append(m.cas, theCA)
	m.Unlock()
	err = theCA.Start(m.config.Timeout)
	if err != nil {
		return err
	}
	conn, err := ca.Connect(theCA)
	if err != nil {
		return err
	}
	defer conn.Close()
	id, err := conn.Enroll(fmt.Sprintf("%s CA Admin", organizationName), "admin", "adminpw")
	if err != nil {
		return err
	}
	organization.SetCAAdmin(id)
	logger.Printf("Created and started CA for endorsing organization %s", organization.Name())
	return nil
}

func (m *Microfab) createChannel(config Channel) (*common.Block, error) {
	logger.Printf("Creating channel %s ...", config.Name)
	capabilityLevel := config.CapabilityLevel
	if capabilityLevel == "" {
		capabilityLevel = m.config.CapabilityLevel
	}
	opts := []channel.Option{
		channel.WithCapabilityLevel(capabilityLevel),
	}
	for _, endorsingOrganization := range m.endorsingOrganizations {
		found := false
		for _, organizationName := range config.EndorsingOrganizations {
			if endorsingOrganization.Name() == organizationName {
				found = true
				break
			}
		}
		if found {
			opts = append(opts, channel.AddMSPID(endorsingOrganization.MSPID()))
		}
	}
	err := channel.CreateChannel(m.ordererConnection, config.Name, opts...)
	if err != nil {
		return nil, err
	}
	var genesisBlock *common.Block
	for {
		genesisBlock, err = blocks.GetGenesisBlock(m.ordererConnection, config.Name)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
	opts = []channel.Option{}
	for _, peer := range m.peers {
		found := false
		for _, organizationName := range config.EndorsingOrganizations {
			if peer.Organization().Name() == organizationName {
				found = true
				break
			}
		}
		if found {
			opts = append(opts, channel.AddAnchorPeer(peer.MSPID(), peer.APIHostname(false), peer.APIPort(false)))
		}
	}
	err = channel.UpdateChannel(m.ordererConnection, config.Name, opts...)
	if err != nil {
		return nil, err
	}
	logger.Printf("Created channel %s", config.Name)
	return genesisBlock, nil
}

func (m *Microfab) createAndJoinChannel(config Channel) error {
	logger.Printf("Creating and joining channel %s ...", config.Name)
	genesisBlock, err := m.createChannel(config)
	if err != nil {
		return err
	}
	ctx := context.Background()
	eg, _ := errgroup.WithContext(ctx)
	for i := range m.peers {
		peer := m.peers[i]
		connection := m.peerConnections[i]
		found := false
		for _, organizationName := range config.EndorsingOrganizations {
			if peer.Organization().Name() == organizationName {
				found = true
				break
			}
		}
		if found {
			eg.Go(func() error {
				logger.Printf("Joining channel %s on peer for endorsing organization %s ...", config.Name, peer.Organization().Name())
				err := connection.JoinChannel(genesisBlock)
				if err != nil {
					return err
				}
				logger.Printf("Joined channel %s on peer for endorsing organization %s", config.Name, peer.Organization().Name())
				return nil
			})
		}
	}
	err = eg.Wait()
	if err != nil {
		return err
	}
	logger.Printf("Created and joined channel %s", config.Name)
	return nil
}

func (m *Microfab) stop() error {
	if m.proxy != nil {
		err := m.proxy.Stop()
		if err != nil {
			return err
		}
		m.proxy = nil
	}
	if m.console != nil {
		err := m.console.Stop()
		if err != nil {
			return err
		}
		m.console = nil
	}
	for _, ca := range m.cas {
		err := ca.Stop()
		if err != nil {
			return err
		}
	}
	m.cas = []*ca.CA{}
	for _, peer := range m.peers {
		err := peer.Stop()
		if err != nil {
			return err
		}
	}
	m.peers = []*peer.Peer{}
	for _, couchDBProxy := range m.couchDBProxies {
		err := couchDBProxy.Stop()
		if err != nil {
			return err
		}
	}
	m.couchDBProxies = []*couchdb.Proxy{}
	if m.orderer != nil {
		err := m.orderer.Stop()
		if err != nil {
			return err
		}
		m.orderer = nil
	}
	return nil
}
