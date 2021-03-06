/*
Copyright IBM Corp. 2016 All Rights Reserved.

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

package deliverclient

import (
	"errors"
	"sync"
	"time"

	"fmt"

	"github.com/hyperledger/fabric/core/comm"
	"github.com/hyperledger/fabric/core/deliverservice/blocksprovider"
	"github.com/hyperledger/fabric/protos/orderer"
	"github.com/op/go-logging"
	"github.com/spf13/viper"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var logger *logging.Logger // package-level logger

func init() {
	logger = logging.MustGetLogger("deliveryClient")
}

// DeliverService used to communicate with orderers to obtain
// new block and send the to the committer service
type DeliverService interface {
	// StartDeliverForChannel dynamically starts delivery of new blocks from ordering service
	// to channel peers.
	StartDeliverForChannel(chainID string, ledgerInfo blocksprovider.LedgerInfo) error

	// StopDeliverForChannel dynamically stops delivery of new blocks from ordering service
	// to channel peers.
	StopDeliverForChannel(chainID string) error

	// Stop terminates delivery service and closes the connection
	Stop()
}

// BlocksDelivererFactory the factory interface to create instance
// of BlocksDeliverer interface which capable to bring blocks from
// the ordering service
type BlocksDelivererFactory interface {
	// Create capable to instantiate new BlocksDeliverer
	Create() (blocksprovider.BlocksDeliverer, error)
}

// blocksDelivererFactoryImpl the implementation of the blocks deliverer factory
// holds the reference to the grpc client connection and capable to create new
// grpc stream for ordering service, which will be used to pull out blocks for
// specific chain
type blocksDelivererFactoryImpl struct {
	conn *grpc.ClientConn
}

// Create a factory method which is capable to instantiate new BlocksDeliverer
func (factory *blocksDelivererFactoryImpl) Create() (blocksprovider.BlocksDeliverer, error) {
	var abc orderer.AtomicBroadcast_DeliverClient
	var err error
	abc, err = orderer.NewAtomicBroadcastClient(factory.conn).Deliver(context.TODO())
	if err != nil {
		return nil, err
	}

	return abc, nil
}

// deliverServiceImpl the implementation of the delivery service
// maintains connection to the ordering service and maps of
// blocks providers
type deliverServiceImpl struct {
	clients map[string]blocksprovider.BlocksProvider

	clientsFactory BlocksDelivererFactory

	lock sync.RWMutex

	gossip blocksprovider.GossipServiceAdapter

	stopping bool

	conn *grpc.ClientConn
}

// NewDeliverService construction function to create and initialize
// delivery service instance. It tries to establish connection to
// the specified in the configuration ordering service, in case it
// fails to dial to it, return nil
func NewDeliverService(gossip blocksprovider.GossipServiceAdapter) (DeliverService, error) {
	// TODO: Has to be fixed as ordering service configuration is part of the part of configuration block
	endpoint := viper.GetString("peer.committer.ledger.orderer")
	logger.Infof("Creating delivery service to get blocks from the ordering service, %s", endpoint)

	dialOpts := []grpc.DialOption{grpc.WithInsecure(), grpc.WithTimeout(3 * time.Second), grpc.WithBlock()}

	if comm.TLSEnabled() {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(comm.InitTLSForPeer()))
	} else {
		dialOpts = append(dialOpts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(endpoint, dialOpts...)
	if err != nil {
		logger.Errorf("Cannot dial to %s, because of %s", endpoint, err)
		return nil, err
	}

	return NewFactoryDeliverService(gossip, &blocksDelivererFactoryImpl{conn}, conn), nil
}

// NewFactoryDeliverService construction function to create and initialize
// delivery service instance, with gossip service adapter and customized
// factory to create blocks deliverers.
func NewFactoryDeliverService(gossip blocksprovider.GossipServiceAdapter, factory BlocksDelivererFactory, conn *grpc.ClientConn) DeliverService {
	return &deliverServiceImpl{
		clientsFactory: factory,
		gossip:         gossip,
		clients:        make(map[string]blocksprovider.BlocksProvider),
		conn:           conn,
	}
}

// StartDeliverForChannel starts blocks delivery for channel
// initializes the grpc stream for given chainID, creates blocks provider instance
// that spawns in go routine to read new blocks starting from the position provided by ledger
// info instance.
func (d *deliverServiceImpl) StartDeliverForChannel(chainID string, ledgerInfo blocksprovider.LedgerInfo) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.stopping {
		errMsg := fmt.Sprintf("Delivery service is stopping cannot join a new channel %s", chainID)
		logger.Errorf(errMsg)
		return errors.New(errMsg)
	}
	if _, exist := d.clients[chainID]; exist {
		errMsg := fmt.Sprintf("Delivery service - block provider already exists for %s found, can't start delivery", chainID)
		logger.Errorf(errMsg)
		return errors.New(errMsg)
	} else {
		abc, err := d.clientsFactory.Create()
		if err != nil {
			logger.Errorf("Unable to initialize atomic broadcast, due to %s", err)
			return err
		}
		logger.Debug("This peer will pass blocks from orderer service to other peers")
		d.clients[chainID] = blocksprovider.NewBlocksProvider(chainID, abc, d.gossip)

		if err := d.clients[chainID].RequestBlocks(ledgerInfo); err == nil {
			// Start reading blocks from ordering service in case this peer is a leader for specified chain
			go d.clients[chainID].DeliverBlocks()
		}
	}
	return nil
}

// StopDeliverForChannel stops blocks delivery for channel by stopping channel block provider
func (d *deliverServiceImpl) StopDeliverForChannel(chainID string) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.stopping {
		errMsg := fmt.Sprintf("Delivery service is stopping, cannot stop delivery for channel %s", chainID)
		logger.Errorf(errMsg)
		return errors.New(errMsg)
	}
	if client, exist := d.clients[chainID]; exist {
		client.Stop()
		delete(d.clients, chainID)
		logger.Debug("This peer will stop pass blocks from orderer service to other peers")
	} else {
		errMsg := fmt.Sprintf("Delivery service - no block provider for %s found, can't stop delivery", chainID)
		logger.Errorf(errMsg)
		return errors.New(errMsg)
	}
	return nil
}

// Stop all service and release resources
func (d *deliverServiceImpl) Stop() {
	d.lock.Lock()
	defer d.lock.Unlock()
	// Marking flag to indicate the shutdown of the delivery service
	d.stopping = true
	// Closing grpc connection
	if d.conn != nil {
		d.conn.Close()
	}

	for _, client := range d.clients {
		client.Stop()
	}
}
