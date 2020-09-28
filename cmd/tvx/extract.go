package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/fatih/color"

	"github.com/filecoin-project/lotus/api"
	init_ "github.com/filecoin-project/lotus/chain/actors/builtin/init"
	"github.com/filecoin-project/lotus/chain/actors/builtin/reward"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/vm"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/conformance"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/test-vectors/schema"

	"github.com/ipfs/go-cid"
	"github.com/urfave/cli/v2"
)

type extractOpts struct {
	id     string
	block  string
	class  string
	cid    string
	file   string
	retain string
}

var extractFlags extractOpts

var extractCmd = &cli.Command{
	Name:        "extract",
	Description: "generate a test vector by extracting it from a live chain",
	Action:      runExtract,
	Flags: []cli.Flag{
		&repoFlag,
		&cli.StringFlag{
			Name:        "class",
			Usage:       "class of vector to extract; other required flags depend on the; values: 'message'",
			Value:       "message",
			Destination: &extractFlags.class,
		},
		&cli.StringFlag{
			Name:        "id",
			Usage:       "identifier to name this test vector with",
			Value:       "(undefined)",
			Destination: &extractFlags.id,
		},
		&cli.StringFlag{
			Name:        "block",
			Usage:       "optionally, the block CID the message was included in, to avoid expensive chain scanning",
			Destination: &extractFlags.block,
		},
		&cli.StringFlag{
			Name:        "cid",
			Usage:       "message CID to generate test vector from",
			Required:    true,
			Destination: &extractFlags.cid,
		},
		&cli.StringFlag{
			Name:        "out",
			Aliases:     []string{"o"},
			Usage:       "file to write test vector to",
			Destination: &extractFlags.file,
		},
		&cli.StringFlag{
			Name:        "state-retain",
			Usage:       "state retention policy; values: 'accessed-cids', 'accessed-actors'",
			Value:       "accessed-cids",
			Destination: &extractFlags.retain,
		},
	},
}

func runExtract(c *cli.Context) error {
	// LOTUS_DISABLE_VM_BUF disables what's called "VM state tree buffering",
	// which stashes write operations in a BufferedBlockstore
	// (https://github.com/filecoin-project/lotus/blob/b7a4dbb07fd8332b4492313a617e3458f8003b2a/lib/bufbstore/buf_bstore.go#L21)
	// such that they're not written until the VM is actually flushed.
	//
	// For some reason, the standard behaviour was not working for me (raulk),
	// and disabling it (such that the state transformations are written immediately
	// to the blockstore) worked.
	_ = os.Setenv("LOTUS_DISABLE_VM_BUF", "iknowitsabadidea")

	ctx := context.Background()

	// Make the API client.
	fapi, closer, err := lcli.GetFullNodeAPI(c)
	if err != nil {
		return err
	}
	defer closer()

	return doExtract(ctx, fapi, extractFlags)
}

func doExtract(ctx context.Context, fapi api.FullNode, opts extractOpts) error {
	mcid, err := cid.Decode(opts.cid)
	if err != nil {
		return err
	}

	msg, execTs, incTs, err := resolveFromChain(ctx, fapi, mcid, opts.block)
	if err != nil {
		return fmt.Errorf("failed to resolve message and tipsets from chain: %w", err)
	}

	// get the circulating supply before the message was executed.
	circSupplyDetail, err := fapi.StateCirculatingSupply(ctx, incTs.Key())
	if err != nil {
		return fmt.Errorf("failed while fetching circulating supply: %w", err)
	}

	circSupply := circSupplyDetail.FilCirculating.Int64()

	log.Printf("message was executed in tipset: %s", execTs.Key())
	log.Printf("message was included in tipset: %s", incTs.Key())
	log.Printf("circulating supply at inclusion tipset: %d", circSupply)
	log.Printf("finding precursor messages")

	// Iterate through blocks, finding the one that contains the message and its
	// precursors, if any.
	var allmsgs []*types.Message
	for _, b := range incTs.Blocks() {
		messages, err := fapi.ChainGetBlockMessages(ctx, b.Cid())
		if err != nil {
			return err
		}

		related, found, err := findMsgAndPrecursors(messages, msg)
		if err != nil {
			return fmt.Errorf("invariant failed while scanning messages in block %s: %w", b.Cid(), err)
		}

		if found {
			var mcids []cid.Cid
			for _, m := range related {
				mcids = append(mcids, m.Cid())
			}
			log.Printf("found message in block %s; precursors: %v", b.Cid(), mcids[:len(mcids)-1])
			allmsgs = related
			break
		}

		log.Printf("message not found in block %s; number of precursors found: %d; ignoring block", b.Cid(), len(related))
	}

	if allmsgs == nil {
		// Message was not found; abort.
		return fmt.Errorf("did not find a block containing the message")
	}

	precursors := allmsgs[:len(allmsgs)-1]

	var (
		// create a read-through store that uses ChainGetObject to fetch unknown CIDs.
		pst = NewProxyingStores(ctx, fapi)
		g   = NewSurgeon(ctx, fapi, pst)
	)

	driver := conformance.NewDriver(ctx, schema.Selector{}, conformance.DriverOpts{
		DisableVMFlush: true,
	})

	// this is the root of the state tree we start with.
	root := incTs.ParentState()
	log.Printf("base state tree root CID: %s", root)

	// on top of that state tree, we apply all precursors.
	log.Printf("number of precursors to apply: %d", len(precursors))
	for i, m := range precursors {
		log.Printf("applying precursor %d, cid: %s", i, m.Cid())
		_, root, err = driver.ExecuteMessage(pst.Blockstore, root, execTs.Height(), m, &circSupplyDetail.FilCirculating)
		if err != nil {
			return fmt.Errorf("failed to execute precursor message: %w", err)
		}
	}

	var (
		preroot   cid.Cid
		postroot  cid.Cid
		applyret  *vm.ApplyRet
		carWriter func(w io.Writer) error
		retention = opts.retain
	)

	log.Printf("using state retention strategy: %s", retention)
	switch retention {
	case "accessed-cids":
		tbs, ok := pst.Blockstore.(TracingBlockstore)
		if !ok {
			return fmt.Errorf("requested 'accessed-cids' state retention, but no tracing blockstore was present")
		}

		tbs.StartTracing()

		preroot = root
		applyret, postroot, err = driver.ExecuteMessage(pst.Blockstore, preroot, execTs.Height(), msg, &circSupplyDetail.FilCirculating)
		if err != nil {
			return fmt.Errorf("failed to execute message: %w", err)
		}
		accessed := tbs.FinishTracing()
		carWriter = func(w io.Writer) error {
			return g.WriteCARIncluding(w, accessed, preroot, postroot)
		}

	case "accessed-actors":
		log.Printf("calculating accessed actors")
		// get actors accessed by message.
		retain, err := g.GetAccessedActors(ctx, fapi, mcid)
		if err != nil {
			return fmt.Errorf("failed to calculate accessed actors: %w", err)
		}
		// also append the reward actor and the burnt funds actor.
		retain = append(retain, reward.Address, builtin.BurntFundsActorAddr, init_.Address)
		log.Printf("calculated accessed actors: %v", retain)

		// get the masked state tree from the root,
		preroot, err = g.GetMaskedStateTree(root, retain)
		if err != nil {
			return err
		}
		applyret, postroot, err = driver.ExecuteMessage(pst.Blockstore, preroot, execTs.Height(), msg, &circSupplyDetail.FilCirculating)
		if err != nil {
			return fmt.Errorf("failed to execute message: %w", err)
		}
		carWriter = func(w io.Writer) error {
			return g.WriteCAR(w, preroot, postroot)
		}

	default:
		return fmt.Errorf("unknown state retention option: %s", retention)
	}

	log.Printf("message applied; preroot: %s, postroot: %s", preroot, postroot)
	log.Println("performing sanity check on receipt")

	receipt := &schema.Receipt{
		ExitCode:    int64(applyret.ExitCode),
		ReturnValue: applyret.Return,
		GasUsed:     applyret.GasUsed,
	}

	reporter := new(conformance.LogReporter)
	conformance.AssertMsgResult(reporter, receipt, applyret, "as locally executed")
	if reporter.Failed() {
		log.Println(color.RedString("receipt sanity check failed; aborting"))
		return fmt.Errorf("vector generation aborted")
	}

	log.Println(color.GreenString("receipt sanity check succeeded"))

	log.Println("generating vector")
	msgBytes, err := msg.Serialize()
	if err != nil {
		return err
	}

	var (
		out = new(bytes.Buffer)
		gw  = gzip.NewWriter(out)
	)
	if err := carWriter(gw); err != nil {
		return err
	}
	if err = gw.Flush(); err != nil {
		return err
	}
	if err = gw.Close(); err != nil {
		return err
	}

	version, err := fapi.Version(ctx)
	if err != nil {
		return err
	}

	ntwkName, err := fapi.StateNetworkName(ctx)
	if err != nil {
		return err
	}

	// Write out the test vector.
	vector := schema.TestVector{
		Class: schema.ClassMessage,
		Meta: &schema.Metadata{
			ID: opts.id,
			// TODO need to replace schema.GenerationData with a more flexible
			//  data structure that makes no assumption about the traceability
			//  data that's being recorded; a flexible map[string]string
			//  would do.
			Gen: []schema.GenerationData{
				{Source: fmt.Sprintf("network:%s", ntwkName)},
				{Source: fmt.Sprintf("message:%s", msg.Cid().String())},
				{Source: fmt.Sprintf("inclusion_tipset:%s", incTs.Key().String())},
				{Source: fmt.Sprintf("execution_tipset:%s", execTs.Key().String())},
				{Source: "github.com/filecoin-project/lotus", Version: version.String()}},
		},
		CAR: out.Bytes(),
		Pre: &schema.Preconditions{
			Epoch:      int64(execTs.Height()),
			CircSupply: &circSupply,
			StateTree: &schema.StateTree{
				RootCID: preroot,
			},
		},
		ApplyMessages: []schema.Message{{Bytes: msgBytes}},
		Post: &schema.Postconditions{
			StateTree: &schema.StateTree{
				RootCID: postroot,
			},
			Receipts: []*schema.Receipt{
				{
					ExitCode:    int64(applyret.ExitCode),
					ReturnValue: applyret.Return,
					GasUsed:     applyret.GasUsed,
				},
			},
		},
	}

	output := io.WriteCloser(os.Stdout)
	if file := opts.file; file != "" {
		dir := filepath.Dir(file)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("unable to create directory %s: %w", dir, err)
		}
		output, err = os.Create(file)
		if err != nil {
			return err
		}
		defer output.Close() //nolint:errcheck
		defer log.Printf("wrote test vector to file: %s", file)
	}

	enc := json.NewEncoder(output)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&vector); err != nil {
		return err
	}

	return nil
}

// resolveFromChain queries the chain for the provided message, using the block CID to
// speed up the query, if provided
func resolveFromChain(ctx context.Context, api api.FullNode, mcid cid.Cid, block string) (msg *types.Message, execTs *types.TipSet, incTs *types.TipSet, err error) {
	// Extract the full message.
	msg, err = api.ChainGetMessage(ctx, mcid)
	if err != nil {
		return nil, nil, nil, err
	}

	log.Printf("found message with CID %s: %+v", mcid, msg)

	if block == "" {
		log.Printf("locating message in blockchain")

		// Locate the message.
		msgInfo, err := api.StateSearchMsg(ctx, mcid)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to locate message: %w", err)
		}

		log.Printf("located message at tipset %s (height: %d) with exit code: %s", msgInfo.TipSet, msgInfo.Height, msgInfo.Receipt.ExitCode)

		execTs, incTs, err = fetchThisAndPrevTipset(ctx, api, msgInfo.TipSet)
		return msg, execTs, incTs, err
	}

	bcid, err := cid.Decode(block)
	if err != nil {
		return nil, nil, nil, err
	}

	log.Printf("message inclusion block CID was provided; scanning around it: %s", bcid)

	blk, err := api.ChainGetBlock(ctx, bcid)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get block: %w", err)
	}

	// types.EmptyTSK hints to use the HEAD.
	execTs, err = api.ChainGetTipSetByHeight(ctx, blk.Height+1, types.EmptyTSK)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get message execution tipset: %w", err)
	}

	// walk back from the execTs instead of HEAD, to save time.
	incTs, err = api.ChainGetTipSetByHeight(ctx, blk.Height, execTs.Key())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to get message inclusion tipset: %w", err)
	}

	return msg, execTs, incTs, nil
}

// fetchThisAndPrevTipset returns the full tipset identified by the key, as well
// as the previous tipset. In the context of vector generation, the target
// tipset is the one where a message was executed, and the previous tipset is
// the one where the message was included.
func fetchThisAndPrevTipset(ctx context.Context, api api.FullNode, target types.TipSetKey) (targetTs *types.TipSet, prevTs *types.TipSet, err error) {
	// get the tipset on which this message was "executed" on.
	// https://github.com/filecoin-project/lotus/issues/2847
	targetTs, err = api.ChainGetTipSet(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	// get the previous tipset, on which this message was mined,
	// i.e. included on-chain.
	prevTs, err = api.ChainGetTipSet(ctx, targetTs.Parents())
	if err != nil {
		return nil, nil, err
	}
	return targetTs, prevTs, nil
}

// findMsgAndPrecursors scans the messages in a block to locate the supplied
// message, looking into the BLS or SECP section depending on the sender's
// address type.
//
// It returns any precursors (if they exist), and the found message (if found),
// in a slice.
//
// It also returns a boolean indicating whether the message was actually found.
//
// This function also asserts invariants, and if those fail, it returns an error.
func findMsgAndPrecursors(messages *api.BlockMessages, target *types.Message) (related []*types.Message, found bool, err error) {
	// Decide which block of messages to process, depending on whether the
	// sender is a BLS or a SECP account.
	input := messages.BlsMessages
	if senderKind := target.From.Protocol(); senderKind == address.SECP256K1 {
		input = make([]*types.Message, 0, len(messages.SecpkMessages))
		for _, sm := range messages.SecpkMessages {
			input = append(input, &sm.Message)
		}
	}

	for _, other := range input {
		if other.From != target.From {
			continue
		}

		// this message is from the same sender, so it's related.
		related = append(related, other)

		if other.Nonce > target.Nonce {
			return nil, false, fmt.Errorf("a message with nonce higher than the target was found before the target; offending mcid: %s", other.Cid())
		}

		// this message is the target; we're done.
		if other.Cid() == target.Cid() {
			return related, true, nil
		}
	}

	// this could happen because a block contained related messages, but not
	// the target (that is, messages with a lower nonce, but ultimately not the
	// target).
	return related, false, nil
}
