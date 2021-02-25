package siafile

import (
	"testing"
	"time"

	"gitlab.com/NebulousLabs/Sia/siatest/dependencies"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"
	"gitlab.com/NebulousLabs/writeaheadlog"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
)

// TestSiaFileFaultyDisk simulates interacting with a SiaFile on a faulty disk.
func TestSiaFileFaultyDisk(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	t.Parallel()

	// Determine a reasonable timeout for the test.
	var testTimeout time.Duration
	if testing.Short() {
		t.SkipNow()
	} else if build.VLONG {
		testTimeout = time.Minute
	} else {
		testTimeout = 10 * time.Second
	}

	// Create the dependency.
	fdd := dependencies.NewFaultyDiskDependency(10000) // Fails after 10000 writes.
	fdd.Disable()

	// Create a new blank siafile.
	sf, wal, walPath := newBlankTestFileAndWAL(1)
	sf.deps = fdd

	// Wrap it in a file set entry.
	if err := setCombinedChunkOfTestFile(sf); err != nil {
		t.Fatal(err)
	}

	// Create 50 hostkeys from which to choose from.
	hostkeys := make([]types.SiaPublicKey, 0, 50)
	for i := 0; i < 50; i++ {
		spk := types.SiaPublicKey{}
		fastrand.Read(spk.Key)
		hostkeys = append(hostkeys, types.SiaPublicKey{})
	}

	// The outer loop is responsible for simulating a restart of siad by
	// reloading the wal, applying transactions and loading the sf from disk
	// again.
	fdd.Enable()
	testDone := time.After(testTimeout)
	numRecoveries := 0
	numSuccessfulIterations := 0
OUTER:
	for {
		select {
		case <-testDone:
			break OUTER
		default:
		}

		// The inner loop applies a random number of operations on the file.
		for {
			select {
			case <-testDone:
				break OUTER
			default:
			}
			// 5% chance to break out of inner loop.
			if fastrand.Intn(100) < 5 {
				break
			}
			// 80% chance to add a piece.
			if fastrand.Intn(100) < 80 {
				spk := hostkeys[fastrand.Intn(len(hostkeys))]
				offset := uint64(fastrand.Intn(int(sf.staticMetadata.FileSize)))
				snap, err := sf.Snapshot(modules.RandomSiaPath())
				if err != nil {
					if errors.Contains(err, dependencies.ErrDiskFault) {
						numRecoveries++
						break
					}
					// If the error wasn't caused by the dependency, the test
					// fails.
					t.Fatal(err)
				}
				chunkIndex, _ := snap.ChunkIndexByOffset(offset)
				pieceIndex := uint64(fastrand.Intn(sf.staticMetadata.staticErasureCode.NumPieces()))
				if err := sf.AddPiece(spk, chunkIndex, pieceIndex, crypto.Hash{}); err != nil {
					if errors.Contains(err, dependencies.ErrDiskFault) {
						numRecoveries++
						break
					}
					// If the error wasn't caused by the dependency, the test
					// fails.
					t.Fatal(err)
				}
			}
			numSuccessfulIterations++
		}

		// 20% chance that drive is repaired.
		if fastrand.Intn(100) < 20 {
			fdd.Reset()
		}

		// Try to reload the file. This simulates failures during recovery.
	LOAD:
		for tries := 0; ; tries++ {
			// If we have already tried for 10 times, we reset the dependency
			// to avoid getting stuck here.
			if tries%10 == 0 {
				fdd.Reset()
			}
			// Close existing wal.
			_, err := wal.CloseIncomplete()
			if err != nil {
				t.Fatal(err)
			}
			// Reopen wal.
			var txns []*writeaheadlog.Transaction
			txns, wal, err = writeaheadlog.New(walPath)
			if err != nil {
				t.Fatal(err)
			}
			// Apply unfinished txns.
			for _, txn := range txns {
				if err := applyUpdates(fdd, txn.Updates...); err != nil {
					if errors.Contains(err, dependencies.ErrDiskFault) {
						numRecoveries++
						continue LOAD // try again
					} else {
						t.Fatal(err)
					}
				}
				if err := txn.SignalUpdatesApplied(); err != nil {
					t.Fatal(err)
				}
			}
			// Load file again.
			/*
				 PARTIAL TODO:
						 TODO: Uncomment once we enable partial chunks again
							_, err = loadSiaFile(sf.partialsSiaFile.siaFilePath, wal, fdd)
							if err != nil {
								if errors.Contains(err, dependencies.ErrDiskFault) {
									numRecoveries++
									continue // try again
								} else {
									t.Fatal(err)
								}
							}
			*/
			sf, err = loadSiaFile(sf.siaFilePath, wal, fdd)
			sf.deps = fdd
			if err != nil {
				if errors.Contains(err, dependencies.ErrDiskFault) {
					numRecoveries++
					continue // try again
				} else {
					t.Fatal(err)
				}
			}
			/*
				 PARTIAL TODO:
						partialsEntry := &SiaFileSetEntry{
							dummyEntry(partialsSiaFile),
							uint64(fastrand.Intn(math.MaxInt32)),
						}
			*/
			//sf = dummyEntry(siafile)
			sf.SetPartialsSiaFile(nil)
			break
		}
	}
	t.Logf("Recovered from %v disk failures", numRecoveries)
	t.Logf("Inner loop %v iterations without failures", numSuccessfulIterations)
}
