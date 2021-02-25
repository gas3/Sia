package renter

import (
	"time"

	"gitlab.com/NebulousLabs/Sia/modules"
)

// callStatus returns the status of the worker.
func (w *worker) callStatus() modules.WorkerStatus {
	w.downloadMu.Lock()
	downloadOnCoolDown := w.onDownloadCooldown()
	downloadTerminated := w.downloadTerminated
	downloadQueueSize := len(w.downloadChunks)
	var downloadCoolDownErr string
	if w.downloadRecentFailureErr != nil {
		downloadCoolDownErr = w.downloadRecentFailureErr.Error()
	}
	downloadCoolDownTime := w.downloadRecentFailure.Add(downloadFailureCooldown).Sub(time.Now())
	w.downloadMu.Unlock()

	w.mu.Lock()
	defer w.mu.Unlock()

	uploadOnCoolDown, uploadCoolDownTime := w.onUploadCooldown()
	var uploadCoolDownErr string
	if w.uploadRecentFailureErr != nil {
		uploadCoolDownErr = w.uploadRecentFailureErr.Error()
	}

	maintenanceOnCooldown, maintenanceCoolDownTime, maintenanceCoolDownErr := w.staticMaintenanceState.managedMaintenanceCooldownStatus()
	var mcdErr string
	if maintenanceCoolDownErr != nil {
		mcdErr = maintenanceCoolDownErr.Error()
	}

	// Update the worker cache before returning a status.
	w.staticTryUpdateCache()
	cache := w.staticCache()
	return modules.WorkerStatus{
		// Contract Information
		ContractID:      cache.staticContractID,
		ContractUtility: cache.staticContractUtility,
		HostPubKey:      w.staticHostPubKey,

		// Download information
		DownloadCoolDownError: downloadCoolDownErr,
		DownloadCoolDownTime:  downloadCoolDownTime,
		DownloadOnCoolDown:    downloadOnCoolDown,
		DownloadQueueSize:     downloadQueueSize,
		DownloadTerminated:    downloadTerminated,

		// Upload information
		UploadCoolDownError: uploadCoolDownErr,
		UploadCoolDownTime:  uploadCoolDownTime,
		UploadOnCoolDown:    uploadOnCoolDown,
		UploadQueueSize:     len(w.unprocessedChunks),
		UploadTerminated:    w.uploadTerminated,

		// Job Queues
		DownloadSnapshotJobQueueSize: int(w.staticJobDownloadSnapshotQueue.callStatus().size),
		UploadSnapshotJobQueueSize:   int(w.staticJobUploadSnapshotQueue.callStatus().size),

		// Maintenance Cooldown Information
		MaintenanceOnCooldown:    maintenanceOnCooldown,
		MaintenanceCoolDownError: mcdErr,
		MaintenanceCoolDownTime:  maintenanceCoolDownTime,

		// Account Information
		AccountBalanceTarget: w.staticBalanceTarget,
		AccountStatus:        w.staticAccount.managedStatus(),

		// Price Table Information
		PriceTableStatus: w.staticPriceTableStatus(),

		// Read Job Information
		ReadJobsStatus: w.callReadJobStatus(),

		// HasSector Job Information
		HasSectorJobsStatus: w.callHasSectorJobStatus(),
	}
}

// staticPriceTableStatus returns the status of the worker's price table
func (w *worker) staticPriceTableStatus() modules.WorkerPriceTableStatus {
	pt := w.staticPriceTable()

	var recentErrStr string
	if pt.staticRecentErr != nil {
		recentErrStr = pt.staticRecentErr.Error()
	}

	return modules.WorkerPriceTableStatus{
		ExpiryTime: pt.staticExpiryTime,
		UpdateTime: pt.staticUpdateTime,

		Active: time.Now().Before(pt.staticExpiryTime),

		RecentErr:     recentErrStr,
		RecentErrTime: pt.staticRecentErrTime,
	}
}

// callReadJobStatus returns the status of the read job queue
func (w *worker) callReadJobStatus() modules.WorkerReadJobsStatus {
	jrq := w.staticJobReadQueue
	status := jrq.callStatus()

	var recentErrString string
	if status.recentErr != nil {
		recentErrString = status.recentErr.Error()
	}

	avgJobTimeInMs := func(l uint64) uint64 {
		if d := jrq.callExpectedJobTime(l); d > 0 {
			return uint64(d.Milliseconds())
		}
		return 0
	}

	return modules.WorkerReadJobsStatus{
		AvgJobTime64k:       avgJobTimeInMs(1 << 16),
		AvgJobTime1m:        avgJobTimeInMs(1 << 20),
		AvgJobTime4m:        avgJobTimeInMs(1 << 22),
		ConsecutiveFailures: status.consecutiveFailures,
		JobQueueSize:        status.size,
		RecentErr:           recentErrString,
		RecentErrTime:       status.recentErrTime,
	}
}

// callHasSectorJobStatus returns the status of the has sector job queue
func (w *worker) callHasSectorJobStatus() modules.WorkerHasSectorJobsStatus {
	hsq := w.staticJobHasSectorQueue
	status := hsq.callStatus()

	var recentErrStr string
	if status.recentErr != nil {
		recentErrStr = status.recentErr.Error()
	}

	// Use 30 sectors for the expected job time since that is the default
	// redundancy for files.
	var avgJobTimeInMs uint64 = 0
	if d := hsq.callExpectedJobTime(uint64(modules.RenterDefaultDataPieces + modules.RenterDefaultParityPieces)); d > 0 {
		avgJobTimeInMs = uint64(d.Milliseconds())
	}

	return modules.WorkerHasSectorJobsStatus{
		AvgJobTime:          avgJobTimeInMs,
		ConsecutiveFailures: status.consecutiveFailures,
		JobQueueSize:        status.size,
		RecentErr:           recentErrStr,
		RecentErrTime:       status.recentErrTime,
	}
}
