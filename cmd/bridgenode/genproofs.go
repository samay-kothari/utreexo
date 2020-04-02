package bridge

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/mit-dci/utreexo/cmd/ttl"
	"github.com/mit-dci/utreexo/cmd/util"
	"github.com/mit-dci/utreexo/utreexo"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// build the bridge node / proofs
func BuildProofs(
	net wire.BitcoinNet, ttlpath, offsetfile string, sig chan bool) error {

	// Channel to alert the tell the main loop it's ok to exit
	done := make(chan bool, 1)

	// Waitgroup to alert stopBuildProofs() that revoffet and offset has
	// been finished
	offsetFinished := make(chan bool, 1)

	// Channel for stopBuildProofs() to wait
	finish := make(chan bool, 1)

	// Handle user interruptions
	go stopBuildProofs(sig, offsetFinished, done, finish)

	// If given the option testnet=true, check if the blk00000.dat file
	// in the directory is a testnet file. Vise-versa for mainnet
	util.CheckNet(net)

	// Creates all the directories needed for bridgenode
	util.MakePaths()

	// Init forest and variables. Resumes if the data directory exists
	forest, height, lastIndexOffsetHeight, pOffset, err :=
		initBridgeNodeState(net, offsetFinished)
	if err != nil {
		panic(err)
	}

	// Open leveldb
	o := new(opt.Options)
	o.CompactionTableSizeMultiplier = 8
	lvdb, err := leveldb.OpenFile(ttlpath, o)
	if err != nil {
		panic(err)
	}
	defer lvdb.Close()

	// For ttl value writing
	var batchwg sync.WaitGroup
	batchan := make(chan *leveldb.Batch, 10)

	// Start 16 workers. Just an arbitrary number
	for j := 0; j < 16; j++ {
		go ttl.DbWorker(batchan, lvdb, &batchwg)
	}

	// To send/receive blocks from blockreader()
	blockReadQueue := make(chan util.BlockAndRev, 10)

	// Reads block asynchronously from .dat files
	// Reads util the lastIndexOffsetHeight
	go util.BlockReader(
		blockReadQueue, lastIndexOffsetHeight, height,
		util.OffsetFilePath, util.RevOffsetFilePath)

	// for the pFile
	proofAndHeightChan := make(chan util.ProofAndHeight, 1)
	pFile, err := os.OpenFile(
		util.PFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	pFileBuf := bufio.NewWriter(pFile) // buffered write to file

	// for pOffsetFile
	proofChan := make(chan []byte, 1)
	pOffsetFile, err := os.OpenFile(
		util.POffsetFilePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		panic(err)
	}
	pOffsetFileBuf := bufio.NewWriter(pOffsetFile) // buffered write to file

	var fileWait sync.WaitGroup
	go pFileWorker(proofAndHeightChan, pFileBuf, &fileWait, done)
	go pOffsetFileWorker(proofChan, &pOffset, pOffsetFileBuf, &fileWait, done)

	fmt.Println("Building Proofs and ttldb...")

	var stop bool // bool for stopping the main loop

	for ; height != lastIndexOffsetHeight && stop != true; height++ {

		// Receive txs from the asynchronous blk*.dat reader
		block := <-blockReadQueue

		// Writes the ttl values for each tx to leveldb
		ttl.WriteBlock(block, batchan, &batchwg)

		// generate the adds and dels from the transactions passed in
		// Adds are the TXOs and Dels are the TXINs
		blockAdds, _, blockDels := genAddDel(block)

		// Verify that the TXINs exist in our forest
		// This should never fail within the context of our code and setting
		blockProof, err := genVerifyDels(blockDels, forest, block.Height)
		if err != nil {
			return err
		}

		// convert blockproof struct to bytes
		b := blockProof.ToBytes()

		// Add to WaitGroup and send data to channel to be written
		// to disk
		fileWait.Add(1)
		proofChan <- b

		// Add to WaitGroup and send data to channel to be written
		// to disk
		fileWait.Add(1)
		proofAndHeightChan <- util.ProofAndHeight{
			Proof: b, Height: block.Height}

		// TODO: Don't ignore undoblock
		// Modifies the forest with the given TXINs and TXOUTs
		_, err = forest.Modify(blockAdds, blockProof.Targets)
		if err != nil {
			return err
		}

		if block.Height%10000 == 0 {
			fmt.Println("On block :", block.Height+1)
		}

		// Check if stopSig is no longer false
		// stop = true makes the loop exit
		select {
		case stop = <-done: // receives true from stopBuildProofs()
		default:
		}
	}

	// wait until dbWorker() has written to the ttldb file
	// allows leveldb to close gracefully
	batchwg.Wait()

	// Wait for the file workers to finish
	fileWait.Wait()
	pFileBuf.Flush()
	pOffsetFileBuf.Flush()

	// Save the current state so genproofs can be resumed
	err = saveBridgeNodeData(forest, pOffset, height)
	if err != nil {
		panic(err)
	}

	fmt.Println("Done writing")

	// Tell stopBuildProofs that it's ok to exit
	finish <- true
	return nil

}

// pFileWorker takes in blockproof and height information from the channel
// and writes to disk. MUST NOT have more than one worker as the proofs need to be
// in order
func pFileWorker(blockProofAndHeight chan util.ProofAndHeight,
	pFileBuf *bufio.Writer, fileWait *sync.WaitGroup, done chan bool) {

	for {

		bp := <-blockProofAndHeight

		var writebyte []byte
		// U32tB always returns 4 bytes
		// Later this could also be changed to magic bytes
		writebyte = append(writebyte,
			utreexo.U32tB(uint32(bp.Height+1))...)

		// write the size of the proof
		writebyte = append(writebyte,
			utreexo.U32tB(uint32(len(bp.Proof)))...)

		// Write the actual proof
		writebyte = append(writebyte, bp.Proof...)

		_, err := pFileBuf.Write(writebyte)
		if err != nil {
			panic(err)
		}
		fileWait.Done()
	}
}

// pOffsetFileWorker receives proofs from the channel and writes to disk
// aynschornously. MUST NOT have more than one worker as the proofoffsets need to be
// in order.
func pOffsetFileWorker(proofChan chan []byte, pOffset *int32,
	pOffsetFileBuf *bufio.Writer, fileWait *sync.WaitGroup, done chan bool) {

	for {
		bp := <-proofChan

		var writebyte []byte
		writebyte = append(writebyte, util.I32tB(*pOffset)...)

		// Updates the global proof offset. Need for resuming purposes
		*pOffset += int32(len(bp)) + int32(8) // add 8 for height bytes and load size

		_, err := pOffsetFileBuf.Write(writebyte)
		if err != nil {
			panic(err)
		}

		fileWait.Done()
	}

}

// genVerifyDels is a wrapper around forest.ProveBlock and forest.VerifyBlockProof
func genVerifyDels(dels []utreexo.Hash, f *utreexo.Forest, height int32) (
	utreexo.BlockProof, error) {

	// generate block proof. Errors if the tx cannot be proven
	// Should never error out with genproofs as it takes
	// blk*.dat files which have already been vetted by Bitcoin Core
	blockProof, err := f.ProveBlock(dels)
	if err != nil {
		return blockProof, fmt.Errorf("ProveBlock failed at block %d %s %s",
			height+1, f.Stats(), err.Error())
	}

	// Sanity check. Should never fail
	ok := f.VerifyBlockProof(blockProof)
	if !ok {
		return blockProof,
			fmt.Errorf("VerifyBlockProof failed at block %d", height+1)
	}

	return blockProof, nil
}

// genAddDel is a wrapper around genAdds and genDels. It calls those both and
// throws out all the same block spends.
func genAddDel(block util.BlockAndRev) (
	blockAdds []utreexo.LeafTXO, dataLeaves []util.LeafData,
	blockDels []utreexo.Hash) {

	blockDels = genDels(block)
	blockAdds, dataLeaves = genAdds(block)

	numins := 0
	for skipcb, tx := range block.Txs {
		if skipcb == 0 {
			continue
		}
		for _, in := range tx.MsgTx().TxIn {
			fmt.Printf("spend %s\n", in.PreviousOutPoint.String())
		}
		numins += len(tx.MsgTx().TxIn)
	}

	revtxs := len(block.Rev.Block.Tx)
	if numins != 0 {
		fmt.Printf("\t\tblock %d (off by 1?)\n", block.Height)
	}
	match := true
	if numins != revtxs {
		fmt.Printf("?ERROR? block %d %d inputs but %d revs\n",
			block.Height, numins, revtxs)
		match = false
	}
	if revtxs != 0 {
		// fmt.Printf("block %d has rev data:\n", block.Height)
		// what's in a revblock?
		for i, tx := range block.Rev.Block.Tx {
			for j, in := range tx.TxIn {
				if match {

				}

				fmt.Printf("REV tx %d in %x h %d amt %d pks %x\n",
					i, j, in.Height, in.Amount, in.PKScript)
			}
		}
	}

	// Forget all utxos that get spent on the same block
	// they are created.
	utreexo.DedupeHashSlices(&blockAdds, &blockDels)
	return
}

// genAdds generates leafTXOs to be added to the Utreexo forest. These are TxOuts
// Skips all the OP_RETURN transactions
func genAdds(bl util.BlockAndRev) (hashleaves []utreexo.LeafTXO,
	dataleaves []util.LeafData) {
	bh := bl.Blockhash
	cheight := bl.Height << 1 // *2 because of the weird coinbase bit thing
	for coinbaseif0, tx := range bl.Txs {
		// cache txid aka txhash
		txid := tx.MsgTx().TxHash()
		for i, out := range tx.MsgTx().TxOut {
			// Skip all the OP_RETURNs
			if util.IsUnspendable(out) {
				continue
			}
			var l util.LeafData
			l.BlockHash = bh
			l.Outpoint.Hash = txid
			l.Outpoint.Index = uint32(i)
			l.CbHeight = cheight
			if coinbaseif0 == 0 {
				l.CbHeight |= 1
			}
			l.Amt = out.Value
			l.PkScript = out.PkScript

			// before shipping off the hash, save leafdata into DB
			// TODO this is super redundant and should be done with rev
			// data or gettxout instead
			dataleaves = append(dataleaves, l)

			var uleaf utreexo.LeafTXO
			uleaf.Hash = l.LeafHash()
			hashleaves = append(hashleaves, uleaf)
		}
	}
	return
}

// genDels generates txs to be deleted from the Utreexo forest. These are TxIns
func genDels(block util.BlockAndRev) (blockDels []utreexo.Hash) {
	// for coinbaseif0, tx := range block.Txs {
	// if coinbaseif0 == 0 {
	// continue
	// }

	// for _, in := range tx.MsgTx().TxIn {
	// Grab TXID of the tx that created this TXIN
	// op := in.PreviousOutPoint.String()

	// look up outpoint here, either with rev data or gettxout

	// }
	// }
	return
}

// stopBuildProofs listens for the signal from the OS and initiates an exit squence
func stopBuildProofs(
	sig, offsetfinished, done, finish chan bool) {

	// Listen for SIGINT, SIGQUIT, SIGTERM
	<-sig

	// Sometimes there are bugs that make the program run forver.
	// Utreexo binary should never take more than 10 seconds to exit
	go func() {
		time.Sleep(10 * time.Second)
		fmt.Println("Program timed out. Force quitting. Data likely corrupted")
		os.Exit(1)
	}()

	// Tell the user that the sig is received
	fmt.Println("User exit signal received. Exiting...")

	select {
	// If offsetfile is there or was built, don't remove it
	case <-offsetfinished:
		select {
		default:
			done <- true
		}
	// If nothing is received, delete offsetfile and other directories
	// Don't wait for done channel from the main BuildProofs() for loop
	default:
		select {
		default:
			fmt.Println("offsetfile incomplete, removing...")
			// May not work sometimes.
			err := os.RemoveAll(util.OffsetDirPath)
			if err != nil {
				fmt.Println("ERR. offsetdata/ directory not removed. Please manually remove it.")
			}
			err = os.RemoveAll(util.RevOffsetDirPath)
			if err != nil {
				fmt.Println("ERR. revdata/ directory not removed. Please manually remove it.")
			}
			fmt.Println("Exiting...")
			os.Exit(0)
		}
	}

	// Wait until BuildProofs() or buildOffsetFile() says it's ok to exit
	<-finish
	os.Exit(0)
}