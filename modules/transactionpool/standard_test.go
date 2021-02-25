package transactionpool

import (
	"testing"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"

	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
)

// TestIntegrationLargeTransactions tries to add a large transaction to the
// transaction pool.
func TestIntegrationLargeTransactions(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}

	tpt, err := createTpoolTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tpt.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	// Create a large transaction and try to get it accepted.
	arbData := make([]byte, modules.TransactionSizeLimit)
	copy(arbData, modules.PrefixNonSia[:])
	fastrand.Read(arbData[100:116]) // prevents collisions with other transacitons in the loop.
	txn := types.Transaction{ArbitraryData: [][]byte{arbData}}
	err = tpt.tpool.AcceptTransactionSet([]types.Transaction{txn})
	if !errors.Contains(err, modules.ErrLargeTransaction) {
		t.Fatal(err)
	}

	// Create a large transaction set and try to get it accepted.
	var tset []types.Transaction
	for i := 0; i <= modules.TransactionSetSizeLimit/10e3; i++ {
		arbData := make([]byte, 10e3)
		copy(arbData, modules.PrefixNonSia[:])
		fastrand.Read(arbData[100:116]) // prevents collisions with other transacitons in the loop.
		txn := types.Transaction{ArbitraryData: [][]byte{arbData}}
		tset = append(tset, txn)
	}
	err = tpt.tpool.AcceptTransactionSet(tset)
	if !errors.Contains(err, modules.ErrLargeTransactionSet) {
		t.Fatal(err)
	}
}
