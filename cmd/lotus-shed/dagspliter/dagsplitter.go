//go:generate go run ./gen

package dagspliter

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/docker/go-units"
	"github.com/filecoin-project/lotus/lib/blockstore"
	"github.com/filecoin-project/lotus/lib/ipfsbstore"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	mdag "github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-unixfs"
	uio "github.com/ipfs/go-unixfs/io"
	"github.com/ipld/go-car"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
)

/// Box is a way of packing together *partial* DAGs to achieve a certain size
/// while generating the associated CAR file containing them. It is an
/// alternative to actual re-chunking the DAG nodes which can be expensive for
/// very large DAGs. A *partial* DAG is generated by excluding certain sub-DAGs
/// from it.
type Box struct {
	/// CID of the roots of the *partial* DAGs contained in this Box.
	Roots []cid.Cid
	/// CIDs of the roots of the sub-DAGs excluded from the original DAGs
	/// (delimited by Roots). We don't keep track of which sub-DAG is being
	/// trimmed from which full DAG in Roots, so to obtain the *partial* DAGs
	/// one needs to walk each DAG in Roots checking if any of its links
	/// are contained here.
	External []cid.Cid
}

func (box *Box) isExternal(link *ipld.Link) bool {
	// Boxes we're working on are likely to be in L2/L3 cache, and comparing bytes
	// is really fast, so it may not even make sense to optimize this, at least
	// unless it shows up in traces.
	for _, externalLink := range box.External {
		if bytes.Equal(externalLink.Bytes(), link.Cid.Bytes()) {
			return true
		}
	}
	return false
}

type builder struct {
	// Service to fetch the nodes in the DAGs and query its links.
	dagService ipld.DAGService

	// Maximum size allowed for each generated Box.
	boxMaxSize uint64

	// Minimum size of graph chunks to bother packing into boxes
	minSubgraphSize uint64

	// Generated boxes when packing a DAG.
	boxes []*Box
	// Used size of the current box we are packing (last one in the list). Since
	// we only pack one box at a time and don't come back to a box once we're
	// done with it we just track a single value here and not in each box.
	boxUsedSize uint64
}

func getSingleNodeSize(node ipld.Node) uint64 {
	// FIXME: How to check the size of the parent node without taking into
	//  account the children? The Node interface doesn't seem to account for
	//  that so we are going directly to the Block interface for now.
	//  We can probably get away with not accounting non-file data well, and
	//  just have some % overhead when accounting space (obviously that will
	//  break horribly with small files, but it should be good enough in the
	//  average case).
	return uint64(len(node.RawData()))
}

func (b *builder) getTreeSize(nd ipld.Node) (uint64, error) {
	switch n := nd.(type) {
	case *mdag.RawNode:
		return uint64(len(n.RawData())), nil

	case *mdag.ProtoNode:
		fsNode, err := unixfs.FSNodeFromBytes(n.Data())
		if err != nil {
			return 0, xerrors.Errorf("loading unixfs node: %w", err)
		}

		switch fsNode.Type() {
		case unixfs.TFile, unixfs.TRaw, unixfs.TDirectory, unixfs.THAMTShard:
			return n.Size()
		case unixfs.TMetadata:
			/*if len(n.Links()) == 0 {
				return nil, xerrors.New("incorrectly formatted metadata object")
			}
			child, err := n.Links()[0].GetNode(ctx, b.dagService)
			if err != nil {
				return nil, err
			}

			childpb, ok := child.(*mdag.ProtoNode)
			if !ok {
				return nil, mdag.ErrNotProtobuf
			}*/

			return 0, xerrors.Errorf("metadata object support todo")
		case unixfs.TSymlink:
			return 0, xerrors.Errorf("symlink object support todo")
		default:
			return 0, unixfs.ErrUnrecognizedType
		}
	default:
		return 0, uio.ErrUnkownNodeType
	}
}

// Get current box we are packing into. By definition now this is always the
// last created box.
func (b *builder) boxID() int {
	return len(b.boxes) - 1
}

// Get current box we are packing into.
// FIXME: Make sure from the construction of the builder that there is always one.
func (b *builder) box() *Box {
	return b.boxes[b.boxID()]
}

func (b *builder) newBox() {
	b.boxes = append(b.boxes, new(Box))
	b.boxUsedSize = 0
}

// Remaining size in the current box.
// FIXME: Since we allow to pack nodes bigger than box size this might
//  return a negative value if we over-packed. This is not nice as we
//  end up mixing signed and unsigned values, for now this is contained
//  in `fits()` only.
func (b *builder) boxRemainingSize() int64 {
	return int64(b.boxMaxSize) - int64(b.used())
}

func (b *builder) used() uint64 {
	return b.boxUsedSize
}

func (b *builder) print(msg string) {
	status := fmt.Sprintf("[BOX %d] <%s>:",
		b.boxID(), units.BytesSize(float64(b.used())))
	fmt.Fprintf(os.Stderr, "%s %s\n", status, msg)
}

func (b *builder) emptyBox() bool {
	// FIXME: Assert this is always `0 <= ret <= max_size`.
	return b.used() == 0
}

// Check this size fits in the current box.
func (b *builder) fits(size uint64) bool {
	return int64(size) <= b.boxRemainingSize()
}

func (b *builder) addSize(size uint64) {
	// FIXME: Maybe assert size (`fits`).
	b.boxUsedSize += size
}

func (b *builder) packRoot(c cid.Cid) {
	b.box().Roots = append(b.box().Roots, c)
}

func (b *builder) addExternalLink(node cid.Cid) {
	b.box().External = append(b.box().External, node)
}

// Pack a DAG delimited by `initialRoot` in boxes. To enforce the maximum
// box size the DAG will be decomposed into smaller sub-DAGs if necessary.
func (b *builder) add(ctx context.Context, initialRoot cid.Cid) error {
	// LIFO queue with the roots that need to be scanned and boxed.
	// LIFO(-ish, node links pushed in reverse) should result in slightly better
	// data layout (less fragmentation in leaves) than FIFO.
	rootsToPack := []cid.Cid{initialRoot}

	for len(rootsToPack) > 0 {
		// Pick one root node from the queue.
		root := rootsToPack[len(rootsToPack)-1]
		rootsToPack = rootsToPack[:len(rootsToPack)-1]

		prevNumberOfRoots := len(rootsToPack)
		err := mdag.Walk(ctx,
			// FIXME: Check if this is the standard way of fetching links.
			func(ctx context.Context, c cid.Cid) ([]*ipld.Link, error) {
				return ipld.GetLinks(ctx, b.dagService, c)
			},
			root,
			// FIXME: The `Visit` function can't return errors, which seems odd
			//  given it should be the function that does the core of the walking
			//  logic (besides signaling if we want to continue with the walk or
			//  not). For now everything is a panic here.
			// FIXME: Check for repeated nodes? How do they count in the CAR file?
			func(nodeCid cid.Cid) bool {
				node, err := b.dagService.Get(ctx, nodeCid)
				if err != nil {
					panic(fmt.Sprintf("getting head node: %s", err))
				}

				treeSize, err := b.getTreeSize(node)
				if err != nil {
					panic(fmt.Sprintf("getting tree size: %s", err))
				}

				b.print(fmt.Sprintf("checking node %s, tree size %s",
					nodeCid.String(), units.BytesSize(float64(treeSize))))

				if b.fits(treeSize) {
					b.addSize(treeSize)
					if nodeCid == root {
						b.packRoot(nodeCid)
					}
					// FIXME: Rethink above addSize/packRoot. We pack only the top node
					//  and then just add the size of its children (implicit in DAG).
					b.print("added entire tree to box")

					// The entire (sub-)graph fits so no need to keep walking it.
					return false
				}

				// Too big for the current box. We need to split parent
				// and sub-graphs (from the child nodes) and inspect their
				// sizes separately.

				// First check if we should even bother splitting the graph more
				if treeSize > b.minSubgraphSize {
					// First check the size of the parent node alone.
					parentSize := getSingleNodeSize(node)
					b.print(fmt.Sprintf("tree too big, single node size: %s",
						units.BytesSize(float64(parentSize))))

					if b.fits(parentSize) || b.emptyBox() {
						b.addSize(parentSize)
						if nodeCid == root {
							b.packRoot(nodeCid)
						}
						// Even if the node doesn't fit but this is an empty box we
						// should add it nonetheless. It means it doesn't fit in *any*
						// box so at least make sure it has its own dedicated one.
						b.print("added node to box")

						// Added the parent to the box, now process its children in the
						// next `Walk()` calls.
						return true
					} else {
						b.print(fmt.Sprintf("node too big (%s), adding as root for another box", units.BytesSize(float64(parentSize))))
					}
				} else {
					b.print(fmt.Sprintf("subgraph too small (%s), adding as root for another box", units.BytesSize(float64(treeSize))))
				}

				// Doesn't fit or it doesn't make sense to split the graph more:
				// process this node in the next box as a root.
				rootsToPack = append(rootsToPack, nodeCid)
				b.addExternalLink(nodeCid)
				// No need to visit children as not even the parent fits.
				return false
			},
			// FIXME: We're probably not ready for any type of concurrency at this point.
			mdag.Concurrency(0),
		)
		if err != nil {
			return xerrors.Errorf("error walking dag: %w", err)
		}

		if len(rootsToPack) > prevNumberOfRoots {
			// We have added internal nodes as "new" roots which means we'll
			// need a new box to put them in.
			b.newBox()
			fmt.Fprintf(os.Stderr, "\n***CREATING NEW BOX %d***\n\n", b.boxID())
		}
	}

	return nil
}

type countBs struct {
	blockstore.Blockstore
	get, has int64
}

func (cbs *countBs) Has(c cid.Cid) (bool, error) {
	atomic.AddInt64(&cbs.has, 1)
	return cbs.Blockstore.Has(c)
}

func (cbs *countBs) Get(c cid.Cid) (blocks.Block, error) {
	atomic.AddInt64(&cbs.get, 1)
	return cbs.Blockstore.Get(c)
}

var Cmd = &cli.Command{
	// FIXME: Review command name. Splitting is just *one* of the recourses
	//  to pack a box and generate a CAR file; it's not the *main* thing this does.
	Name:      "dagsplit",
	Usage:     "Pack a DAG into a series of custom size CAR files",
	ArgsUsage: "<DAG root> <CAR size>",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "output-dir",
			Usage: "specify path of directory to write the CAR files into",
			Value: "dagsplitter-car-files",
		},
		&cli.IntFlag{
			Name:  "min-subgraph-size",
			Usage: "minimum size of graph chunks to bother packing into boxes",
			Value: 0,
		},
		&cli.BoolFlag{
			Name:  "breadth-first",
			Usage: "pack in breadth-first order instead of the default depth-first",
		},
	},
	Action: func(cctx *cli.Context) error {
		ctx := cctx.Context

		if cctx.Args().Len() != 2 {
			return xerrors.Errorf("expected 2 args: root and CAR size")
		}

		root, err := cid.Parse(cctx.Args().First())
		if err != nil {
			return xerrors.Errorf("parsing root cid: %w", err)
		}

		chunk, err := units.RAMInBytes(cctx.Args().Get(1))
		if err != nil {
			return xerrors.Errorf("parsing chunk size: %w", err)
		}

		if cctx.Bool("breadth-first") {
			return xerrors.Errorf("breadth-first pack not implemented yet")
		}

		// FIXME: The DAG-to-Box generation and Box-to-CAR generation is now
		//  coupled in the same command, so for now we don't save the intermediate
		//  boxes in the block store (IPFS) but keep them in memory and dump
		//  them directly as CAR files.

		bs, err := ipfsbstore.NewIpfsBstore(ctx, true)
		if err != nil {
			return xerrors.Errorf("getting ipfs bstore: %w", err)
		}
		cbs := &countBs{Blockstore: bs}

		bb := builder{
			dagService:      mdag.NewDAGService(blockservice.New(cbs, nil)),
			boxMaxSize:      uint64(chunk),
			minSubgraphSize: uint64(cctx.Int("minSubgraphSize")),
			boxes:           make([]*Box, 0),
		}
		bb.newBox() // FIXME: Encapsulate in a constructor.

		err = bb.add(ctx, root)
		if err != nil {
			return xerrors.Errorf("error generating boxes: %w", err)
		}

		fmt.Fprintf(os.Stderr, "\nBlockstore access stats: get:%d has:%d\n", cbs.get, cbs.has)

		// =====================
		// CAR generation logic.
		// =====================
		// FIXME: Maybe should be decoupled from the above (probably in its own
		//  separate command).

		// Create output directory if necessary.
		outDir := cctx.String("output-dir")
		if _, err := os.Stat(outDir); os.IsNotExist(err) {
			if err := os.Mkdir(outDir, os.ModePerm); err != nil {
				return xerrors.Errorf("creating directory: %w", err)
			}
		} else if err != nil {
			return xerrors.Errorf("querying directory stat: %w", err)
		}

		// Write one CAR file for each Box.
		fmt.Fprintf(os.Stderr, "\nWriting CAR files to directory %s/:\n", outDir)
		for i, box := range bb.boxes {
			out := new(bytes.Buffer)
			if err := car.WriteCarWithWalker(context.TODO(), bb.dagService, box.Roots, out, BoxCarWalkFunc(box)); err != nil {
				return xerrors.Errorf("write car failed: %w", err)
			}

			boxIdWidth := 1 + int(math.Log10(float64(len(bb.boxes))))
			carFilename := fmt.Sprintf("box-%s-%*d.car", root.String(), boxIdWidth, i)
			fmt.Fprintf(os.Stderr, "%s\t%s\n", units.BytesSize(float64(out.Len())), carFilename)
			err = ioutil.WriteFile(filepath.Join(outDir, carFilename), out.Bytes(), 0644)
			if err != nil {
				return xerrors.Errorf("write file failed: %w", err)
			}
		}

		return nil
	},
}

func BoxCarWalkFunc(box *Box) func(nd ipld.Node) (out []*ipld.Link, err error) {
	return func(nd ipld.Node) (out []*ipld.Link, err error) {
		for _, link := range nd.Links() {

			// Do not walk into nodes external to the current `box`.
			if box.isExternal(link) {
				//_, _ = fmt.Fprintf(os.Stderr, "Found external link, skipping from CAR generation: %s\n", link.Cid.String())
				continue
			}

			// Taken from the original `gen.CarWalkFunc`:
			//  Filecoin sector commitment CIDs (CommD (padded/truncated sha256
			//  binary tree), CommR (basically magic tree)). Those are linked
			//  directly in the chain state, so this avoids trying to accidentally
			//  walk over a few exabytes of data.
			// FIXME: Avoid duplicating this code from the original.
			pref := link.Cid.Prefix()
			if pref.Codec == cid.FilCommitmentSealed || pref.Codec == cid.FilCommitmentUnsealed {
				continue
			}

			out = append(out, link)
		}

		return out, nil
	}
}

// FIXME: Add real test. For now check that the output matches across refactors.
// ```bash
// ./lotus-shed dagsplit QmRLzQZ5efau2kJLfZRm9Guo1DxiBp3xCAVf6EuPCqKdsB 1M`
// Writing CAR files to directory dagsplitter-car-files/:
// 1.007MiB	box-QmRLzQZ5efau2kJLfZRm9Guo1DxiBp3xCAVf6EuPCqKdsB-0.car
// 406.5KiB	box-QmRLzQZ5efau2kJLfZRm9Guo1DxiBp3xCAVf6EuPCqKdsB-1.car
// ```
