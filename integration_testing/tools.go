package integration_testing

import (
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/wire"
	"github.com/deso-protocol/core/cmd"
	"github.com/deso-protocol/core/lib"
	"github.com/dgraph-io/badger/v3"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"io/ioutil"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

// This testing suite is the first serious attempt at making a comprehensive functional testing framework for DeSo nodes.
// To accomplish this, we want to be able to simulate any network topology, as well as network conditions such as
// asynchronous communication, disconnects, partitions, etc. The main toolbox used to make this happen is the
// ConnectionBridge struct, which simulates a node-to-node network connection. More on this later.
//
// Then, we also need some validation tools so we can compare node state during our test cases. For instance, we can
// compare nodes by their databases with compareNodesByDB to check if two nodes have identical databases. We can also
// compare nodes by database checksums via compareNodesByChecksum. It is a good practice to verify both states and checksums.
//
// Finally, we have wrappers around general node behavior, such as startNode, restartNode, etc. We can also wait until
// a node is synced to a certain height with listenForBlockHeight, or until hypersync has begun syncing a certain prefix
// via listenForSyncPrefix.
//
// Summarizing, the node testing framework is intentionally lightweight and general so that we can test a wide range of
// node behaviors. Check out

// Global variable that determines the max tip blockheight of syncing nodes throughout test cases.
const MaxSyncBlockHeight = 1500

// Global variable that allows setting node configuration hypersync snapshot period.
const HyperSyncSnapshotPeriod = 1000

// get a random temporary directory.
func getDirectory(t *testing.T) string {
	require := require.New(t)
	dbDir, err := ioutil.TempDir("", "badgerdb")
	if err != nil {
		require.NoError(err)
	}
	return dbDir
}

// generateConfig creates a default config for a node, with provided port, db directory, and number of max peers.
// It's usually the first step to starting a node.
func generateConfig(t *testing.T, port uint32, dataDir string, maxPeers uint32) *cmd.Config {
	config := &cmd.Config{}
	params := lib.DeSoMainnetParams

	params.DNSSeeds = []string{}
	config.Params = &params
	config.ProtocolPort = uint16(port)
	// "/Users/piotr/data_dirs/n98_1"
	config.DataDirectory = dataDir
	if err := os.MkdirAll(config.DataDirectory, os.ModePerm); err != nil {
		t.Fatalf("Could not create data directories (%s): %v", config.DataDirectory, err)
	}
	config.TXIndex = false
	config.HyperSync = false
	config.MaxSyncBlockHeight = 0
	config.ConnectIPs = []string{}
	config.PrivateMode = true
	config.GlogV = 0
	config.GlogVmodule = "*bitcoin_manager*=0,*balance*=0,*view*=0,*frontend*=0,*peer*=0,*addr*=0,*network*=0,*utils*=0,*connection*=0,*main*=0,*server*=0,*mempool*=0,*miner*=0,*blockchain*=0"
	config.MaxInboundPeers = maxPeers
	config.TargetOutboundPeers = maxPeers
	config.StallTimeoutSeconds = 900
	config.MinFeerate = 1000
	config.OneInboundPerIp = false
	config.MaxBlockTemplatesCache = 100
	config.MaxSyncBlockHeight = 100
	config.MinBlockUpdateInterval = 10
	config.SnapshotBlockHeightPeriod = HyperSyncSnapshotPeriod
	config.MaxSyncBlockHeight = MaxSyncBlockHeight
	config.SyncType = lib.NodeSyncTypeBlockSync
	//config.ArchivalMode = true

	return config
}

// waitForNodeToFullySync will busy-wait until provided node is fully current.
func waitForNodeToFullySync(node *cmd.Node) {
	ticker := time.NewTicker(5 * time.Millisecond)
	for {
		<-ticker.C

		if node.Server.GetBlockchain().ChainState() == lib.SyncStateFullyCurrent {
			if node.Server.GetBlockchain().Snapshot() != nil {
				node.Server.GetBlockchain().Snapshot().WaitForAllOperationsToFinish()
			}
			return
		}
	}
}

// waitForNodeToFullySyncAndStoreAllBlocks will busy-wait until node is fully current and all blocks have been stored.
func waitForNodeToFullySyncAndStoreAllBlocks(node *cmd.Node) {
	ticker := time.NewTicker(5 * time.Millisecond)
	for {
		<-ticker.C

		if node.Server.GetBlockchain().IsFullyStored() {
			if node.Server.GetBlockchain().Snapshot() != nil {
				node.Server.GetBlockchain().Snapshot().WaitForAllOperationsToFinish()
			}
			return
		}
	}
}

// waitForNodeToFullySyncTxIndex will busy-wait until node is fully current and txindex has finished syncing.
func waitForNodeToFullySyncTxIndex(node *cmd.Node) {
	ticker := time.NewTicker(5 * time.Millisecond)
	for {
		<-ticker.C

		if node.TXIndex.FinishedSyncing() && node.Server.GetBlockchain().ChainState() == lib.SyncStateFullyCurrent {
			if node.Server.GetBlockchain().Snapshot() != nil {
				node.Server.GetBlockchain().Snapshot().WaitForAllOperationsToFinish()
			}
			return
		}
	}
}

// compareNodesByChecksum checks if the two provided nodes have identical checksums.
func compareNodesByChecksum(t *testing.T, nodeA *cmd.Node, nodeB *cmd.Node) {
	require := require.New(t)
	checksumA, err := nodeA.Server.GetBlockchain().Snapshot().Checksum.ToBytes()
	require.NoError(err)
	checksumB, err := nodeB.Server.GetBlockchain().Snapshot().Checksum.ToBytes()
	require.NoError(err)

	if !reflect.DeepEqual(checksumA, checksumB) {
		t.Fatalf("compareNodesByChecksum: error checksums not equal checksumA (%v), "+
			"checksumB (%v)", checksumA, checksumB)
	}
	fmt.Printf("Identical checksums: nodeA (%v)\n nodeB (%v)\n", checksumA, checksumB)
}

// compareNodesByState will look through all state records in nodeA and nodeB databases and will compare them.
// The nodes pass this comparison iff they have identical states.
func compareNodesByState(t *testing.T, nodeA *cmd.Node, nodeB *cmd.Node, verbose int) {
	compareNodesByStateWithPrefixList(t, nodeA.ChainDB, nodeB.ChainDB, lib.StatePrefixes.StatePrefixesList, verbose)
}

// compareNodesByDB will look through all records in nodeA and nodeB databases and will compare them.
// The nodes pass this comparison iff they have identical states.
func compareNodesByDB(t *testing.T, nodeA *cmd.Node, nodeB *cmd.Node, verbose int) {
	var prefixList [][]byte
	for prefix := range lib.StatePrefixes.StatePrefixesMap {
		// We skip utxooperations because we actually can't sync them in hypersync.
		if reflect.DeepEqual([]byte{prefix}, lib.Prefixes.PrefixBlockHashToUtxoOperations) {
			continue
		}
		prefixList = append(prefixList, []byte{prefix})
	}
	compareNodesByStateWithPrefixList(t, nodeA.ChainDB, nodeB.ChainDB, prefixList, verbose)
}

// compareNodesByDB will look through all records in nodeA and nodeB txindex databases and will compare them.
// The nodes pass this comparison iff they have identical states.
func compareNodesByTxIndex(t *testing.T, nodeA *cmd.Node, nodeB *cmd.Node, verbose int) {
	var prefixList [][]byte
	for prefix := range lib.StatePrefixes.StatePrefixesMap {
		// We skip utxooperations because we actually can't sync them in hypersync.
		if reflect.DeepEqual([]byte{prefix}, lib.Prefixes.PrefixBlockHashToUtxoOperations) {
			continue
		}
		prefixList = append(prefixList, []byte{prefix})
	}
	compareNodesByStateWithPrefixList(t, nodeA.TXIndex.TXIndexChain.DB(), nodeB.TXIndex.TXIndexChain.DB(), prefixList, verbose)
}

// compareNodesByDB will look through all records in provided prefixList in nodeA and nodeB databases and will compare them.
// The nodes pass this comparison iff they have identical states.
func compareNodesByStateWithPrefixList(t *testing.T, dbA *badger.DB, dbB *badger.DB, prefixList [][]byte, verbose int) {
	maxBytes := lib.SnapshotBatchSize
	var brokenPrefixes [][]byte
	var broken bool
	sort.Slice(prefixList, func(ii, jj int) bool {
		return prefixList[ii][0] < prefixList[jj][0]
	})
	for _, prefix := range prefixList {
		lastPrefix := prefix
		invalidLengths := false
		invalidKeys := false
		invalidValues := false
		invalidFull := false
		existingEntriesDb0 := make(map[string][]byte)
		for {
			// Fetch a state chunk from nodeA database.
			dbEntriesA, isChunkFullA, err := lib.DBIteratePrefixKeys(dbA, prefix, lastPrefix, maxBytes)
			if err != nil {
				t.Fatal(errors.Wrapf(err, "problem reading nodeA database for prefix (%v) last prefix (%v)",
					prefix, lastPrefix))
			}
			for _, entry := range dbEntriesA {
				existingEntriesDb0[hex.EncodeToString(entry.Key)] = entry.Value
			}

			// Fetch a state chunk from nodeB database.
			dbEntriesB, isChunkFullB, err := lib.DBIteratePrefixKeys(dbB, prefix, lastPrefix, maxBytes)
			if err != nil {
				t.Fatal(errors.Wrapf(err, "problem reading nodeB database for prefix (%v) last prefix (%v",
					prefix, lastPrefix))
			}
			for _, entry := range dbEntriesB {
				key := hex.EncodeToString(entry.Key)
				if _, exists := existingEntriesDb0[key]; exists {
					if !reflect.DeepEqual(entry.Value, existingEntriesDb0[key]) {
						if !invalidValues || verbose >= 1 {
							glog.Errorf("Databases not equal on prefix: %v, the key is (%v); "+
								"unequal values (db0, db1) : (%v, %v)\n", prefix, entry.Key,
								entry.Value, existingEntriesDb0[key])
							invalidValues = true
						}
					}
					delete(existingEntriesDb0, key)
				} else {
					glog.Errorf("Databases not equal on prefix: %v, and key: %v; the entry in database B "+
						"was not found in the existingEntriesMap, and has value: %v\n", prefix, key, entry.Value)
				}
			}

			// Make sure we've fetched the same number of entries for nodeA and nodeB.
			if len(dbEntriesA) != len(dbEntriesB) {
				invalidLengths = true
				glog.Errorf("Databases not equal on prefix: %v, and lastPrefix: %v;"+
					"varying lengths (nodeA, nodeB) : (%v, %v)\n", prefix, lastPrefix, len(dbEntriesA), len(dbEntriesB))
			}

			// It doesn't matter which map we iterate through, since if we got here it means they have
			// an identical number of unique keys. So we will choose dbEntriesA for convenience.
			for ii, entry := range dbEntriesA {
				if ii >= len(dbEntriesB) {
					break
				}
				if !reflect.DeepEqual(entry.Key, dbEntriesB[ii].Key) {
					if !invalidKeys || verbose >= 1 {
						glog.Errorf("Databases not equal on prefix: %v, and lastPrefix: %v; unequal keys "+
							"(nodeA, nodeB) : (%v, %v)\n", prefix, lastPrefix, entry.Key, dbEntriesB[ii].Key)
						invalidKeys = true
					}
				}
			}
			//for ii, entry := range dbEntriesA {
			//	if ii >= len(dbEntriesB) {
			//		break
			//	}
			//	if !reflect.DeepEqual(entry.Value, dbEntriesB[ii].Value) {
			//		if !invalidValues || verbose >= 1 {
			//			glog.Errorf("Databases not equal on prefix: %v, and key: %v; the key is (%v); "+
			//				"unequal values len (db0, db1) : (%v, %v)\n", prefix, entry.Key, entry.Key,
			//				len(entry.Value), len(dbEntriesB[ii].Value))
			//			glog.Errorf("Databases not equal on prefix: %v, and lastPrefix: %v; unequal values "+
			//				"(nodeA, nodeB) : (%v, %v)\n", prefix, lastPrefix, entry.Value, dbEntriesB[ii].Value)
			//			invalidValues = true
			//		}
			//	}
			//}

			// Make sure the isChunkFull match for both chunks.
			if isChunkFullA != isChunkFullB {
				if !invalidFull || verbose >= 1 {
					glog.Errorf("Databases not equal on prefix: %v, and lastPrefix: %v;"+
						"unequal fulls (nodeA, nodeB) : (%v, %v)\n", prefix, lastPrefix, isChunkFullA, isChunkFullB)
					invalidFull = true
				}
			}

			if len(dbEntriesA) > 0 {
				lastPrefix = dbEntriesA[len(dbEntriesA)-1].Key
			} else {
				break
			}

			if !isChunkFullA {
				break
			}
		}
		status := "PASS"
		if invalidLengths || invalidKeys || invalidValues || invalidFull {
			status = "FAIL"
			brokenPrefixes = append(brokenPrefixes, prefix)
			broken = true
		}
		glog.Infof("The number of entries in existsMap for prefix (%v) is (%v)\n", prefix, len(existingEntriesDb0))
		for key, entry := range existingEntriesDb0 {
			glog.Infof("ExistingMape entry: (key, len(value) : (%v, %v)\n", key, len(entry))
		}
		glog.Infof("Status for prefix (%v): (%s)\n invalidLengths: (%v); invalidKeys: (%v); invalidValues: "+
			"(%v); invalidFull: (%v)\n\n", prefix, status, invalidLengths, invalidKeys, invalidValues, invalidFull)
	}
	if broken {
		t.Fatalf("Databases differ! Broken prefixes: %v", brokenPrefixes)
	}
}

// computeNodeStateChecksum goes through node's state records and computes the checksum.
func computeNodeStateChecksum(t *testing.T, node *cmd.Node, blockHeight uint64) []byte {
	require := require.New(t)

	// Get all state prefixes and sort them.
	var prefixes [][]byte
	for prefix, isState := range lib.StatePrefixes.StatePrefixesMap {
		if !isState {
			continue
		}
		prefixes = append(prefixes, []byte{prefix})
	}
	sort.Slice(prefixes, func(ii, jj int) bool {
		return prefixes[ii][0] < prefixes[jj][0]
	})

	carrierChecksum := &lib.StateChecksum{}
	carrierChecksum.Initialize(nil, nil)

	err := node.Server.GetBlockchain().DB().View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		for _, prefix := range prefixes {
			it := txn.NewIterator(opts)
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				item := it.Item()
				key := item.Key()
				err := item.Value(func(value []byte) error {
					return carrierChecksum.AddOrRemoveBytesWithMigrations(key, value, blockHeight,
						nil, true)
				})
				if err != nil {
					return err
				}
			}
			it.Close()
		}
		return nil
	})
	require.NoError(err)
	require.NoError(carrierChecksum.Wait())
	checksumBytes, err := carrierChecksum.ToBytes()
	require.NoError(err)
	return checksumBytes
}

// Stop the provided node.
func shutdownNode(t *testing.T, node *cmd.Node) *cmd.Node {
	if !node.IsRunning {
		t.Fatalf("shutdownNode: can't shutdown, node is already down")
	}

	node.Stop()
	config := node.Config
	return cmd.NewNode(config)
}

// Start the provided node.
func startNode(t *testing.T, node *cmd.Node) *cmd.Node {
	if node.IsRunning {
		t.Fatalf("startNode: node is already running")
	}
	// Start the node.
	node.Start()
	t.Cleanup(func() {
		node.Stop()
	})
	return node
}

// Restart the provided node.A
func restartNode(t *testing.T, node *cmd.Node) *cmd.Node {
	if !node.IsRunning {
		t.Fatalf("shutdownNode: can't restart, node already down")
	}

	newNode := shutdownNode(t, node)
	return startNode(t, newNode)
}

// listenForBlockHeight busy-waits until the node's block tip reaches provided height.
func listenForBlockHeight(t *testing.T, node *cmd.Node, height uint32, signal chan<- bool) {
	ticker := time.NewTicker(1 * time.Millisecond)
	go func() {
		for {
			<-ticker.C
			if node.Server.GetBlockchain().BlockTip().Height >= height {
				signal <- true
				break
			}
		}
	}()
}

// disconnectAtBlockHeight busy-waits until the node's block tip reaches provided height, and then disconnects
// from the provided bridge.
func disconnectAtBlockHeight(t *testing.T, syncingNode *cmd.Node, bridge *ConnectionBridge, height uint32) {
	listener := make(chan bool)
	listenForBlockHeight(t, syncingNode, height, listener)
	<-listener
	bridge.Disconnect()
}

// restartAtHeightAndReconnectNode will restart the node once it syncs to the provided height, and then reconnects
// the node to the source.
func restartAtHeightAndReconnectNode(t *testing.T, node *cmd.Node, source *cmd.Node, currentBridge *ConnectionBridge,
	height uint32) (_node *cmd.Node, _bridge *ConnectionBridge) {

	require := require.New(t)
	disconnectAtBlockHeight(t, node, currentBridge, height)
	newNode := restartNode(t, node)
	// Wait after the restart.
	time.Sleep(1 * time.Second)
	fmt.Println("Restarted")
	// bridge the nodes together.
	bridge := NewConnectionBridge(newNode, source)
	require.NoError(bridge.Start())
	return newNode, bridge
}

// listenForSyncPrefix will wait until the node starts downloading the provided syncPrefix in hypersync, and then sends
// a message to the provided signal channel.
func listenForSyncPrefix(t *testing.T, node *cmd.Node, syncPrefix []byte, signal chan<- bool) {
	ticker := time.NewTicker(1 * time.Millisecond)
	go func() {
		for {
			<-ticker.C
			for _, prefix := range node.Server.HyperSyncProgress.PrefixProgress {
				if reflect.DeepEqual(prefix.Prefix, syncPrefix) {
					//if reflect.DeepEqual(prefix.LastReceivedKey, syncPrefix) {
					//	break
					//}
					signal <- true
					return
				}
			}
		}
	}()
}

// disconnectAtSyncPrefix will busy-wait until node starts downloading the provided syncPrefix in hypersync, and then
// it will disconnect the node from the provided bridge.
func disconnectAtSyncPrefix(t *testing.T, syncingNode *cmd.Node, bridge *ConnectionBridge, syncPrefix []byte) {
	listener := make(chan bool)
	listenForSyncPrefix(t, syncingNode, syncPrefix, listener)
	<-listener
	bridge.Disconnect()
}

// restartAtSyncPrefixAndReconnectNode will
func restartAtSyncPrefixAndReconnectNode(t *testing.T, node *cmd.Node, source *cmd.Node, currentBridge *ConnectionBridge,
	syncPrefix []byte) (_node *cmd.Node, _bridge *ConnectionBridge) {

	require := require.New(t)
	disconnectAtSyncPrefix(t, node, currentBridge, syncPrefix)
	newNode := restartNode(t, node)

	// bridge the nodes together.
	bridge := NewConnectionBridge(newNode, source)
	require.NoError(bridge.Start())
	return newNode, bridge
}

func randomUint32Between(t *testing.T, min, max uint32) uint32 {
	require := require.New(t)
	randomNumber, err := wire.RandomUint64()
	require.NoError(err)
	randomHeight := uint32(randomNumber) % (max - min)
	return randomHeight + min
}
