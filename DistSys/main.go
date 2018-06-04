package main

import (
	"errors"
	"fmt"
	"github.com/DistributedClocks/GoVector/govec"
	"github.com/sbinet/go-python"
	"log"
	"net"
	"net/rpc"
	"strconv"
	"sync"
	"time"
	"os"
	"flag"

)

const (
	basePort     int           = 8000
	myIP         string        = "127.0.0.1:"
	verifierIP   string        = "127.0.0.1:"
	timeoutNS    time.Duration = 10000000000
	numVerifiers int           = 1
)

var (

	//Input arguments
	datasetName   string
	numberOfNodes int

	client Honest

	ensureRPC      chan error
	portsToConnect []string
	clusterPorts   []string

	//Locks
	updateLock    sync.Mutex
	boolLock      sync.Mutex
	convergedLock sync.Mutex

	// global shared variables
	updateSent     bool
	converged      bool
	verifier       bool
	iterationCount = -1

	//Logging
	errLog *log.Logger = log.New(os.Stderr, "[err] ", log.Lshortfile|log.LUTC|log.Lmicroseconds)
	outLog *log.Logger = log.New(os.Stderr, "[peer] ", log.Lshortfile|log.LUTC|log.Lmicroseconds)
	logger *govec.GoLog

	//Errors
	staleError error = errors.New("Stale Update/Block")
)

// Python init function for go-python
func init() {
	err := python.Initialize()
	if err != nil {
		panic(err.Error())
	}
}

// RPC CALLS

type Peer int

// The peer receives an update from another peer if its a verifier in that round.
// The verifier peer takes in the update and returns immediately.
// It calls a separate go-routine for collecting updates and sending updates when all updates have been collected
// Returns:
// - StaleError if its an update for a preceding round.

func (s *Peer) VerifyUpdate(update Update, _ignored *bool) error {

	outLog.Printf("Got update message, iteration %d\n", update.Iteration)

	if update.Iteration < iterationCount {
		handleErrorFatal("Update of previous iteration received", staleError)
		return staleError
	}

	for update.Iteration > iterationCount {
		outLog.Printf("Blocking. Got update for %d, I am at %d\n", update.Iteration, iterationCount)
		time.Sleep(1000 * time.Millisecond)
	}

	go processUpdate(update)

	return nil

}

// The peer receives a block from the verifier of that round.
// It takes in the block and returns immediately.
// It calls a separate go-routine for appending the block as part of its chain
// Returns:
// - staleError if its an block for a preceding round.

func (s *Peer) RegisterBlock(block Block, _ignored *bool) error {

	outLog.Printf("Got block message, iteration %d\n", block.Data.Iteration)

	if block.Data.Iteration < iterationCount {
		handleErrorFatal("Block of previous iteration received", staleError)
		return staleError
	}

	for block.Data.Iteration > iterationCount {
		outLog.Printf("Blocking. Got block for %d, I am at %d\n", block.Data.Iteration, iterationCount)
		time.Sleep(1000 * time.Millisecond)
	}

	go addBlockToChain(block)

	return nil

}

// Basic check to see if you are the verifier in the next round

func amVerifier(nodeNum int) bool {

	//TODO: THIS WILL CHANGE AS OUR VRF APPROACH MATURES.
	if (iterationCount % numberOfNodes) == client.id {
		return true
	} else {
		return false
	}

}

// Dummy placeholder VRF function

func VRF(iterationCount int) []string {

	// TODO: THIS WILL CHANGE AS THE VRF IMPLEMENTATION CHANGES
	verifiers := make([]string, numVerifiers)
	verifiers[0] = strconv.Itoa(basePort + (iterationCount % numberOfNodes))
	return verifiers

}

// Error handling

func handleErrorFatal(msg string, e error) {

	if e != nil {
		errLog.Fatalf("%s, err = %s\n", msg, e.Error())
	}

}

func exitOnError(prefix string, err error) {

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s, err = %s\n", prefix, err.Error())
		os.Exit(1)
	}
}

// Parse args, read dataset and initialize separate threads for listening for updates/blocks and sending updates

func main() {

	//Parsing arguments nodeIndex, numberOfNodes, datasetname
	numberOfNodesPtr := flag.Int("t", 0 , "The total number of nodes in the network")

	nodeNumPtr := flag.Int("i", -1 ,"The node's index in the total. Has to be greater than 0")

	datasetNamePtr := flag.String("d", "" , "The name of the dataset to be used")

	flag.Parse()

	nodeNum := *nodeNumPtr
	numberOfNodes = *numberOfNodesPtr
	datasetName = *datasetNamePtr

	if(numberOfNodes <= 0 || nodeNum < 0 || datasetName == ""){
		flag.PrintDefaults()
		os.Exit(1)	
	}

	logger = govec.InitGoVector(os.Args[1], os.Args[1])


	// getports of all other clients in the system
	myPort := strconv.Itoa(nodeNum + basePort)

	for i := 0; i < numberOfNodes; i++ {
		if strconv.Itoa(basePort+i) == myPort {
			continue
		}
		clusterPorts = append(clusterPorts, strconv.Itoa(basePort+i))
	}

	
	//Initialize a honest client
	client = Honest{id: nodeNum, blockUpdates: make([]Update, 0, 5)}

	
	// Reading data and declaring some global locks to be used later
	client.initializeData(datasetName, numberOfNodes)
	converged = false
	verifier = false	
	updateLock = sync.Mutex{}
	boolLock = sync.Mutex{}
	convergedLock = sync.Mutex{}
	ensureRPC = make(chan error)


	// Initializing RPC Server
	peer := new(Peer)
	peerServer := rpc.NewServer()
	peerServer.Register(peer)

	prepareForNextIteration()

	go messageListener(peerServer, myPort)
	messageSender(clusterPorts)

}

// At the start of each iteration, this function is called to reset shared global variables
// based on whether you are a verifier or not.

func prepareForNextIteration() {

	convergedLock.Lock()

	if converged {

		convergedLock.Unlock()
		time.Sleep(1000 * time.Millisecond)
		client.bc.PrintChain()
		os.Exit(1)
	}

	convergedLock.Unlock()

	boolLock.Lock()

	if verifier {
		updateLock.Lock()
		client.flushUpdates(numberOfNodes)
		updateLock.Unlock()
	}

	iterationCount++

	verifier = amVerifier(client.id)

	if verifier {
		outLog.Printf("I am verifier. IterationCount:%d", iterationCount)
		updateSent = true
	} else {
		outLog.Printf("I am not verifier IterationCount:%d", iterationCount)
		updateSent = false
	}

	boolLock.Unlock()

	portsToConnect = make([]string, len(clusterPorts))
	copy(portsToConnect, clusterPorts)

}

// Thread that listens for incoming RPC Calls

func messageListener(peerServer *rpc.Server, port string) {

	l, e := net.Listen("tcp", myIP+port)
	handleErrorFatal("listen error", e)

	outLog.Printf("Peer started. Receiving on %s\n", port)

	for {
		conn, _ := l.Accept()
		outLog.Printf("Accepted new Connection")
		go peerServer.ServeConn(conn)
	}

}

// go routine to process the update received by non verifying nodes

func processUpdate(update Update) {

	updateLock.Lock()
	numberOfUpdates := client.addBlockUpdate(update)
	updateLock.Unlock()

	if numberOfUpdates == (numberOfNodes - 1) {
		blockToSend := client.createBlock(iterationCount)
		sendBlock(blockToSend)
	}


}

// Verifier broadcasts the block of this iteration to all peers

func sendBlock(block Block) {	

	outLog.Printf("Sending block. Iteration: %d\n", block.Data.Iteration)

	// create a thread for separate calling
	
	for _, port := range clusterPorts {
		go callRegisterBlockRPC(block, port)
	}
	
	//check for convergence, wait for RPC calls to return and move to the new iteration

	convergedLock.Lock()
	converged = client.checkConvergence()
	convergedLock.Unlock()

	ensureRPCCallsReturn()
	prepareForNextIteration()

}

// output from channel to ensure all RPC calls to broadcast block are successful

func ensureRPCCallsReturn() {

	for i := 0; i < (numberOfNodes - 1); i++ {
		<-ensureRPC
	}

}

// RPC call to send block to one peer

func callRegisterBlockRPC(block Block, port string) {

	var ign bool
	c := make(chan error)

	conn, er := rpc.Dial("tcp", myIP+port) 
	exitOnError("rpc Dial", er)
	defer conn.Close()

	go func() { c <- conn.Call("Peer.RegisterBlock", block, &ign) }()
	select {
	case err := <-c:

		outLog.Printf("Block sent to verifiee successful")
		handleErrorFatal("Error in sending update", err)
		ensureRPC <- err

		// use err and result
	case <-time.After(timeoutNS):

		fmt.Println("Timeout. Sending Block. Retrying...")
		callRegisterBlockRPC(block, port)
	}

}

// go-routine to process a block received and add to chain. 
// Move to next iteration when done

func addBlockToChain(block Block) {

	client.bc.AddBlockMsg(block)

	if block.Data.Iteration == iterationCount {
		boolLock.Lock()
		updateSent = true
		boolLock.Unlock()
	}

	convergedLock.Lock()
	converged = client.checkConvergence()
	convergedLock.Unlock()
	prepareForNextIteration()

}

// Main sending thread. Checks if you are a non-verifier in the current itearation 
// Sends update if thats the case.
// TODO: Replace with channels for cleanliness

func messageSender(ports []string) {

	for {

		if verifier {

			time.Sleep(100 * time.Millisecond)
			continue
		}

		boolLock.Lock()

		if !updateSent {

			outLog.Printf("Computing Update\n")

			client.computeUpdate(iterationCount, datasetName)

			portsToConnect = VRF(iterationCount)

			for _, port := range portsToConnect {
				go sendUpdateToVerifier(port)
				if iterationCount == client.update.Iteration {
					updateSent = true
				}
			}

			boolLock.Unlock()

		} else {

			boolLock.Unlock()
			time.Sleep(100 * time.Millisecond)

		}

	}

}

// Make RPC call to send update to verifier

func sendUpdateToVerifier(port string) {

	var ign bool
	c := make(chan error)

	conn, err := rpc.Dial("tcp", verifierIP+port)
	defer conn.Close()
	handleErrorFatal("Unable to connect to verifier", err)
	outLog.Printf("Making RPC Call to Verifier. Sending Update, Iteration:%d\n", client.update.Iteration)

	go func() { c <- conn.Call("Peer.VerifyUpdate", client.update, &ign) }()
	select {
	case err := <-c:
		outLog.Printf("Update sent successfully")
		handleErrorFatal("Error in sending update", err)
		// use err and result
	case <-time.After(timeoutNS):

		conn.Close()
		outLog.Printf("Timeout. Sending Update. Retrying...")
		sendUpdateToVerifier(port)
	}

}