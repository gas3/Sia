package renter

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/errors"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/renter/filesystem/siadir"
	"gitlab.com/NebulousLabs/Sia/modules/renter/filesystem/siafile"
)

// bubbleStatus indicates the status of a bubble being executed on a
// directory
type bubbleStatus int

// bubbledSiaDirMetadata is a wrapper for siadir.Metadata that also contains the
// siapath for convenience.
type bubbledSiaDirMetadata struct {
	sp modules.SiaPath
	siadir.Metadata
}

// bubbledSiaFileMetadata is a wrapper for siafile.BubbledMetadata that also
// contains the siapath for convenience.
type bubbledSiaFileMetadata struct {
	sp modules.SiaPath
	bm siafile.BubbledMetadata
}

// bubbleError, bubbleInit, bubbleActive, and bubblePending are the constants
// used to determine the status of a bubble being executed on a directory
const (
	bubbleError bubbleStatus = iota
	bubbleActive
	bubblePending
)

// managedPrepareBubble will add a bubble to the bubble map. If 'true' is returned, the
// caller should proceed by calling bubble. If 'false' is returned, the caller
// should not bubble, another thread will handle running the bubble.
func (r *Renter) managedPrepareBubble(siaPath modules.SiaPath) bool {
	r.bubbleUpdatesMu.Lock()
	defer r.bubbleUpdatesMu.Unlock()

	// Check for bubble in bubbleUpdate map
	siaPathStr := siaPath.String()
	status, ok := r.bubbleUpdates[siaPathStr]
	if !ok {
		r.bubbleUpdates[siaPathStr] = bubbleActive
		return true
	}
	if status != bubbleActive && status != bubblePending {
		build.Critical("bubble status set to bubbleError")
	}
	r.bubbleUpdates[siaPathStr] = bubblePending
	return false
}

// managedCalculateDirectoryMetadata calculates the new values for the
// directory's metadata and tracks the value, either worst or best, for each to
// be bubbled up
func (r *Renter) managedCalculateDirectoryMetadata(siaPath modules.SiaPath) (siadir.Metadata, error) {
	// Set default metadata values to start
	now := time.Now()
	metadata := siadir.Metadata{
		AggregateHealth:              siadir.DefaultDirHealth,
		AggregateLastHealthCheckTime: now,
		AggregateMinRedundancy:       math.MaxFloat64,
		AggregateModTime:             time.Time{},
		AggregateNumFiles:            uint64(0),
		AggregateNumStuckChunks:      uint64(0),
		AggregateNumSubDirs:          uint64(0),
		AggregateRemoteHealth:        siadir.DefaultDirHealth,
		AggregateSize:                uint64(0),
		AggregateStuckHealth:         siadir.DefaultDirHealth,

		Health:              siadir.DefaultDirHealth,
		LastHealthCheckTime: now,
		MinRedundancy:       math.MaxFloat64,
		ModTime:             time.Time{},
		NumFiles:            uint64(0),
		NumStuckChunks:      uint64(0),
		NumSubDirs:          uint64(0),
		RemoteHealth:        siadir.DefaultDirHealth,
		Size:                uint64(0),
		StuckHealth:         siadir.DefaultDirHealth,
	}
	// Read directory
	fileinfos, err := r.staticFileSystem.ReadDir(siaPath)
	if err != nil {
		r.log.Printf("WARN: Error in reading files in directory %v : %v\n", siaPath.String(), err)
		return siadir.Metadata{}, err
	}

	// Iterate over directory and collect the file and dir siapaths.
	var fileSiaPaths, dirSiaPaths []modules.SiaPath
	for _, fi := range fileinfos {
		// Check to make sure renter hasn't been shutdown
		select {
		case <-r.tg.StopChan():
			return siadir.Metadata{}, err
		default:
		}
		// Sort by file and dirs.
		ext := filepath.Ext(fi.Name())
		if ext == modules.SiaFileExtension {
			// SiaFile found.
			fName := strings.TrimSuffix(fi.Name(), modules.SiaFileExtension)
			fileSiaPath, err := siaPath.Join(fName)
			if err != nil {
				r.log.Println("unable to join siapath with dirpath while calculating directory metadata:", err)
				continue
			}
			fileSiaPaths = append(fileSiaPaths, fileSiaPath)
		} else if fi.IsDir() {
			// Directory is found, read the directory metadata file
			dirSiaPath, err := siaPath.Join(fi.Name())
			if err != nil {
				r.log.Println("unable to join siapath with dirpath while calculating directory metadata:", err)
				continue
			}
			dirSiaPaths = append(dirSiaPaths, dirSiaPath)
		}
	}

	// Calculate the Files' bubbleMetadata first.
	// Note: We don't need to abort on error. It's likely that only one or a few
	// files failed and that the remaining metadatas are good to use.
	bubbledMetadatas, err := r.managedCalculateFileMetadatas(fileSiaPaths)
	if err != nil {
		r.log.Printf("failed to calculate file metadata: %v", err)
	}

	// Get all the Directory Metadata
	// Note: We don't need to abort on error. It's likely that only one or a few
	// directories failed and that the remaining metadatas are good to use.
	dirMetadatas, err := r.managedDirectoryMetadatas(dirSiaPaths)
	if err != nil {
		r.log.Printf("failed to calculate file metadata: %v", err)
	}

	for len(bubbledMetadatas)+len(dirMetadatas) > 0 {
		// Aggregate Fields
		var aggregateHealth, aggregateRemoteHealth, aggregateStuckHealth, aggregateMinRedundancy float64
		var aggregateLastHealthCheckTime, aggregateModTime time.Time
		if len(bubbledMetadatas) > 0 {
			// Get next file's metadata.
			bubbledMetadata := bubbledMetadatas[0]
			bubbledMetadatas = bubbledMetadatas[1:]
			fileSiaPath := bubbledMetadata.sp
			fileMetadata := bubbledMetadata.bm
			// If 75% or more of the redundancy is missing, register an alert
			// for the file.
			uid := string(fileMetadata.UID)
			if maxHealth := math.Max(fileMetadata.Health, fileMetadata.StuckHealth); maxHealth >= AlertSiafileLowRedundancyThreshold {
				r.staticAlerter.RegisterAlert(modules.AlertIDSiafileLowRedundancy(uid), AlertMSGSiafileLowRedundancy,
					AlertCauseSiafileLowRedundancy(fileSiaPath, maxHealth, fileMetadata.Redundancy),
					modules.SeverityWarning)
			} else {
				r.staticAlerter.UnregisterAlert(modules.AlertIDSiafileLowRedundancy(uid))
			}

			// If the file's LastHealthCheckTime is still zero, set it as now since it
			// it currently being checked.
			//
			// The LastHealthCheckTime is not a field that is initialized when a file
			// is created, so we can reach this point by one of two ways. If a file is
			// created in the directory after the health loop has decided it needs to
			// be bubbled, or a file is created in a directory that gets a bubble
			// called on it outside of the health loop before the health loop as been
			// able to set the LastHealthCheckTime.
			if fileMetadata.LastHealthCheckTime.IsZero() {
				fileMetadata.LastHealthCheckTime = time.Now()
			}

			// Record Values that compare against sub directories
			aggregateHealth = fileMetadata.Health
			aggregateStuckHealth = fileMetadata.StuckHealth
			aggregateMinRedundancy = fileMetadata.Redundancy
			aggregateLastHealthCheckTime = fileMetadata.LastHealthCheckTime
			aggregateModTime = fileMetadata.ModTime
			if !fileMetadata.OnDisk {
				aggregateRemoteHealth = fileMetadata.Health
			}

			// Update aggregate fields.
			metadata.AggregateNumFiles++
			metadata.AggregateNumStuckChunks += fileMetadata.NumStuckChunks
			metadata.AggregateSize += fileMetadata.Size

			// Update siadir fields.
			metadata.Health = math.Max(metadata.Health, fileMetadata.Health)
			if fileMetadata.LastHealthCheckTime.Before(metadata.LastHealthCheckTime) {
				metadata.LastHealthCheckTime = fileMetadata.LastHealthCheckTime
			}
			if fileMetadata.Redundancy != -1 {
				metadata.MinRedundancy = math.Min(metadata.MinRedundancy, fileMetadata.Redundancy)
			}
			if fileMetadata.ModTime.After(metadata.ModTime) {
				metadata.ModTime = fileMetadata.ModTime
			}
			metadata.NumFiles++
			metadata.NumStuckChunks += fileMetadata.NumStuckChunks
			if !fileMetadata.OnDisk {
				metadata.RemoteHealth = math.Max(metadata.RemoteHealth, fileMetadata.Health)
			}
			metadata.Size += fileMetadata.Size
			metadata.StuckHealth = math.Max(metadata.StuckHealth, fileMetadata.StuckHealth)
		} else if len(dirMetadatas) > 0 {
			// Get next dir's metadata.
			dirMetadata := dirMetadatas[0]
			dirMetadatas = dirMetadatas[1:]

			// Check if the directory's AggregateLastHealthCheckTime is Zero. If so
			// set the time to now and call bubble on that directory to try and fix
			// the directories metadata.
			//
			// The LastHealthCheckTime is not a field that is initialized when
			// a directory is created, so we can reach this point if a directory is
			// created and gets a bubble called on it outside of the health loop
			// before the health loop has been able to set the LastHealthCheckTime.
			if dirMetadata.AggregateLastHealthCheckTime.IsZero() {
				dirMetadata.AggregateLastHealthCheckTime = time.Now()
				err = r.tg.Launch(func() {
					r.callThreadedBubbleMetadata(dirMetadata.sp)
				})
				if err != nil {
					r.log.Printf("WARN: unable to launch bubble for '%v'", dirMetadata.sp)
				}
			}

			// Record Values that compare against files
			aggregateHealth = dirMetadata.AggregateHealth
			aggregateStuckHealth = dirMetadata.AggregateStuckHealth
			aggregateMinRedundancy = dirMetadata.AggregateMinRedundancy
			aggregateLastHealthCheckTime = dirMetadata.AggregateLastHealthCheckTime
			aggregateModTime = dirMetadata.AggregateModTime
			aggregateRemoteHealth = dirMetadata.AggregateRemoteHealth

			// Update aggregate fields.
			metadata.AggregateNumFiles += dirMetadata.AggregateNumFiles
			metadata.AggregateNumStuckChunks += dirMetadata.AggregateNumStuckChunks
			metadata.AggregateNumSubDirs += dirMetadata.AggregateNumSubDirs
			metadata.AggregateSize += dirMetadata.AggregateSize

			// Add 1 to the AggregateNumSubDirs to account for this subdirectory.
			metadata.AggregateNumSubDirs++

			// Update siadir fields
			metadata.NumSubDirs++
		}
		// Track the max value of aggregate health values
		metadata.AggregateHealth = math.Max(metadata.AggregateHealth, aggregateHealth)
		metadata.AggregateRemoteHealth = math.Max(metadata.AggregateRemoteHealth, aggregateRemoteHealth)
		metadata.AggregateStuckHealth = math.Max(metadata.AggregateStuckHealth, aggregateStuckHealth)
		// Track the min value for AggregateMinRedundancy
		if aggregateMinRedundancy != -1 {
			metadata.AggregateMinRedundancy = math.Min(metadata.AggregateMinRedundancy, aggregateMinRedundancy)
		}
		// Update LastHealthCheckTime
		if aggregateLastHealthCheckTime.Before(metadata.AggregateLastHealthCheckTime) {
			metadata.AggregateLastHealthCheckTime = aggregateLastHealthCheckTime
		}
		// Update ModTime
		if aggregateModTime.After(metadata.AggregateModTime) {
			metadata.AggregateModTime = aggregateModTime
		}
	}

	// Sanity check on ModTime. If mod time is still zero it means there were no
	// files or subdirectories. Set ModTime to now since we just updated this
	// directory
	if metadata.AggregateModTime.IsZero() {
		metadata.AggregateModTime = time.Now()
	}
	if metadata.ModTime.IsZero() {
		metadata.ModTime = time.Now()
	}
	// Sanity check on Redundancy. If MinRedundancy is still math.MaxFloat64
	// then set it to -1 to indicate an empty directory
	if metadata.AggregateMinRedundancy == math.MaxFloat64 {
		metadata.AggregateMinRedundancy = -1
	}
	if metadata.MinRedundancy == math.MaxFloat64 {
		metadata.MinRedundancy = -1
	}

	return metadata, nil
}

// managedCalculateFileMetadata calculates and returns the necessary metadata
// information of a siafiles that needs to be bubbled.
func (r *Renter) managedCalculateFileMetadata(siaPath modules.SiaPath, hostOfflineMap, hostGoodForRenewMap map[string]bool) (bubbledSiaFileMetadata, error) {
	// Open SiaFile in a read only state so that it doesn't need to be
	// closed
	sf, err := r.staticFileSystem.OpenSiaFile(siaPath)
	if err != nil {
		return bubbledSiaFileMetadata{}, err
	}
	defer func() {
		err = errors.Compose(err, sf.Close())
	}()

	// Calculate file health
	health, stuckHealth, _, _, numStuckChunks := sf.Health(hostOfflineMap, hostGoodForRenewMap)

	// Calculate file Redundancy and check if local file is missing and
	// redundancy is less than one
	redundancy, _, err := sf.Redundancy(hostOfflineMap, hostGoodForRenewMap)
	if err != nil {
		return bubbledSiaFileMetadata{}, err
	}
	_, err = os.Stat(sf.LocalPath())
	onDisk := err == nil
	if !onDisk && redundancy < 1 {
		r.log.Debugf("File not found on disk and possibly unrecoverable: LocalPath %v; SiaPath %v", sf.LocalPath(), siaPath.String())
	}
	return bubbledSiaFileMetadata{
		sp: siaPath,
		bm: siafile.BubbledMetadata{
			Health:              health,
			LastHealthCheckTime: sf.LastHealthCheckTime(),
			ModTime:             sf.ModTime(),
			NumStuckChunks:      numStuckChunks,
			OnDisk:              onDisk,
			Redundancy:          redundancy,
			Size:                sf.Size(),
			StuckHealth:         stuckHealth,
			UID:                 sf.UID(),
		},
	}, nil
}

// managedCalculateFileMetadatas calculates and returns the necessary metadata
// information of multiple siafiles that need to be bubbled. Usually the return
// value of a method is ignored when the returned error != nil. For
// managedCalculateFileMetadatas we make an exception. The caller can decide
// themselves whether to use the output in case of an error or not.
func (r *Renter) managedCalculateFileMetadatas(siaPaths []modules.SiaPath) (_ []bubbledSiaFileMetadata, err error) {
	/// Get cached offline and goodforrenew maps.
	hostOfflineMap, hostGoodForRenewMap, _, _ := r.managedRenterContractsAndUtilities()

	// Define components
	mds := make([]bubbledSiaFileMetadata, 0, len(siaPaths))
	siaPathChan := make(chan modules.SiaPath, numBubbleWorkerThreads)
	var errs error
	var errMu, mdMu sync.Mutex

	// Create function for loading SiaFiles and calculating the metadata
	metadataWorker := func() {
		for siaPath := range siaPathChan {
			md, err := r.managedCalculateFileMetadata(siaPath, hostOfflineMap, hostGoodForRenewMap)
			if err != nil {
				errMu.Lock()
				errs = errors.Compose(errs, err)
				errMu.Unlock()
				continue
			}
			mdMu.Lock()
			mds = append(mds, md)
			mdMu.Unlock()
		}
	}

	// Launch Metadata workers
	var wg sync.WaitGroup
	for i := 0; i < numBubbleWorkerThreads; i++ {
		wg.Add(1)
		go func() {
			metadataWorker()
			wg.Done()
		}()
	}
	for _, siaPath := range siaPaths {
		siaPathChan <- siaPath
	}
	close(siaPathChan)
	wg.Wait()
	return mds, errs
}

// managedCompleteBubbleUpdate completes the bubble update and updates and/or
// removes it from the renter's bubbleUpdates.
//
// TODO: bubbleUpdatesMu is in violation of conventions, needs to be moved to
// its own object to have its own mu.
func (r *Renter) managedCompleteBubbleUpdate(siaPath modules.SiaPath) {
	r.bubbleUpdatesMu.Lock()
	defer r.bubbleUpdatesMu.Unlock()

	// Check current status
	siaPathStr := siaPath.String()
	status, exists := r.bubbleUpdates[siaPathStr]

	// If the status is 'bubbleActive', delete the status and return.
	if status == bubbleActive {
		delete(r.bubbleUpdates, siaPathStr)
		return
	}
	// If the status is not 'bubbleActive', and the status is also not
	// 'bubblePending', this is an error. There should be a status, and it
	// should either be active or pending.
	if status != bubblePending {
		build.Critical("invalid bubble status", status, exists)
		delete(r.bubbleUpdates, siaPathStr) // Attempt to reset the corrupted state.
		return
	}
	// The status is bubblePending, switch the status to bubbleActive.
	r.bubbleUpdates[siaPathStr] = bubbleActive

	// Launch a thread to do another bubble on this directory, as there was a
	// bubble pending waiting for the current bubble to complete.
	err := r.tg.Add()
	if err != nil {
		return
	}
	go func() {
		defer r.tg.Done()
		r.managedPerformBubbleMetadata(siaPath)
	}()
}

// managedDirectoryMetadatas returns all the metadatas of the SiaDirs for the
// provided siaPaths
func (r *Renter) managedDirectoryMetadatas(siaPaths []modules.SiaPath) ([]bubbledSiaDirMetadata, error) {
	// Define components
	mds := make([]bubbledSiaDirMetadata, 0, len(siaPaths))
	siaPathChan := make(chan modules.SiaPath, numBubbleWorkerThreads)
	var errs error
	var errMu, mdMu sync.Mutex

	// Create function for getting the directory metadata
	metadataWorker := func() {
		for siaPath := range siaPathChan {
			md, err := r.managedDirectoryMetadata(siaPath)
			if err != nil {
				errMu.Lock()
				errs = errors.Compose(errs, err)
				errMu.Unlock()
				continue
			}
			mdMu.Lock()
			mds = append(mds, bubbledSiaDirMetadata{
				siaPath,
				md,
			})
			mdMu.Unlock()
		}
	}

	// Launch Metadata workers
	var wg sync.WaitGroup
	for i := 0; i < numBubbleWorkerThreads; i++ {
		wg.Add(1)
		go func() {
			metadataWorker()
			wg.Done()
		}()
	}
	for _, siaPath := range siaPaths {
		siaPathChan <- siaPath
	}
	close(siaPathChan)
	wg.Wait()
	return mds, errs
}

// managedDirectoryMetadata reads the directory metadata and returns the bubble
// metadata
func (r *Renter) managedDirectoryMetadata(siaPath modules.SiaPath) (_ siadir.Metadata, err error) {
	// Check for bad paths and files
	fi, err := r.staticFileSystem.Stat(siaPath)
	if err != nil {
		return siadir.Metadata{}, err
	}
	if !fi.IsDir() {
		return siadir.Metadata{}, fmt.Errorf("%v is not a directory", siaPath)
	}

	//  Open SiaDir
	siaDir, err := r.staticFileSystem.OpenSiaDirCustom(siaPath, true)
	if err != nil {
		return siadir.Metadata{}, err
	}
	defer func() {
		err = errors.Compose(err, siaDir.Close())
	}()

	// Grab the metadata.
	return siaDir.Metadata()
}

// managedUpdateLastHealthCheckTime updates the LastHealthCheckTime and
// AggregateLastHealthCheckTime fields of the directory metadata by reading all
// the subdirs of the directory.
func (r *Renter) managedUpdateLastHealthCheckTime(siaPath modules.SiaPath) error {
	// Read directory
	fileinfos, err := r.staticFileSystem.ReadDir(siaPath)
	if err != nil {
		r.log.Printf("WARN: Error in reading files in directory %v : %v\n", siaPath.String(), err)
		return err
	}

	// Iterate over directory and find the oldest AggregateLastHealthCheckTime
	aggregateLastHealthCheckTime := time.Now()
	for _, fi := range fileinfos {
		// Check to make sure renter hasn't been shutdown
		select {
		case <-r.tg.StopChan():
			return err
		default:
		}
		// Check for SiaFiles and Directories
		if fi.IsDir() {
			// Directory is found, read the directory metadata file
			dirSiaPath, err := siaPath.Join(fi.Name())
			if err != nil {
				return err
			}
			dirMetadata, err := r.managedDirectoryMetadata(dirSiaPath)
			if err != nil {
				return err
			}
			// Update AggregateLastHealthCheckTime.
			if dirMetadata.AggregateLastHealthCheckTime.Before(aggregateLastHealthCheckTime) {
				aggregateLastHealthCheckTime = dirMetadata.AggregateLastHealthCheckTime
			}
		} else {
			// Ignore everything that is not a directory since files should be updated
			// already by the ongoing bubble.
			continue
		}
	}

	// Write changes to disk.
	entry, err := r.staticFileSystem.OpenSiaDir(siaPath)
	if err != nil {
		return err
	}
	err = entry.UpdateLastHealthCheckTime(aggregateLastHealthCheckTime, time.Now())
	return errors.Compose(err, entry.Close())
}

// callThreadedBubbleMetadata is the thread safe method used to call
// managedBubbleMetadata when the call does not need to be blocking
func (r *Renter) callThreadedBubbleMetadata(siaPath modules.SiaPath) {
	if err := r.tg.Add(); err != nil {
		return
	}
	defer r.tg.Done()
	if err := r.managedBubbleMetadata(siaPath); err != nil {
		r.log.Debugln("WARN: error with bubbling metadata:", err)
	}
}

// managedPerformBubbleMetadata will bubble the metadata without checking the
// bubble preparation.
func (r *Renter) managedPerformBubbleMetadata(siaPath modules.SiaPath) (err error) {
	// Make sure we call callThreadedBubbleMetadata on the parent once we are
	// done.
	defer func() error {
		// Complete bubble
		r.managedCompleteBubbleUpdate(siaPath)

		// Continue with parent dir if we aren't in the root dir already.
		if siaPath.IsRoot() {
			return nil
		}
		parentDir, err := siaPath.Dir()
		if err != nil {
			return errors.AddContext(err, "failed to defer callThreadedBubbleMetadata on parent dir")
		}
		go r.callThreadedBubbleMetadata(parentDir)
		return nil
	}()

	// Calculate the new metadata values of the directory
	metadata, err := r.managedCalculateDirectoryMetadata(siaPath)
	if err != nil {
		e := fmt.Sprintf("could not calculate the metadata of directory %v", siaPath.String())
		return errors.AddContext(err, e)
	}

	// Update directory metadata with the health information. Don't return here
	// to avoid skipping the repairNeeded and stuckChunkFound signals.
	siaDir, err := r.staticFileSystem.OpenSiaDir(siaPath)
	if err != nil {
		e := fmt.Sprintf("could not open directory %v", siaPath.String())
		err = errors.AddContext(err, e)
	} else {
		defer func() {
			err = errors.Compose(err, siaDir.Close())
		}()
		err = siaDir.UpdateBubbledMetadata(metadata)
		if err != nil {
			e := fmt.Sprintf("could not update the metadata of the directory %v", siaPath.String())
			err = errors.AddContext(err, e)
		}
	}

	// If we are at the root directory then check if any files were found in
	// need of repair or and stuck chunks and trigger the appropriate repair
	// loop. This is only done at the root directory as the repair and stuck
	// loops start at the root directory so there is no point triggering them
	// until the root directory is updated
	if siaPath.IsRoot() {
		if metadata.AggregateHealth >= RepairThreshold {
			select {
			case r.uploadHeap.repairNeeded <- struct{}{}:
			default:
			}
		}
		if metadata.AggregateNumStuckChunks > 0 {
			select {
			case r.uploadHeap.stuckChunkFound <- struct{}{}:
			default:
			}
		}
	}
	return err
}

// managedBubbleMetadata calculates the updated values of a directory's metadata
// and updates the siadir metadata on disk then calls callThreadedBubbleMetadata
// on the parent directory so that it is only blocking for the current directory
func (r *Renter) managedBubbleMetadata(siaPath modules.SiaPath) error {
	// Check if bubble is needed
	proceedWithBubble := r.managedPrepareBubble(siaPath)
	if !proceedWithBubble {
		// Update the AggregateLastHealthCheckTime even if we weren't able to
		// bubble right away.
		return r.managedUpdateLastHealthCheckTime(siaPath)
	}
	return r.managedPerformBubbleMetadata(siaPath)
}
