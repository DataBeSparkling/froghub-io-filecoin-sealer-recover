package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	cliutil "github.com/filecoin-project/lotus/cli/util"
	"github.com/filecoin-project/lotus/extern/sector-storage/ffiwrapper"
	"github.com/filecoin-project/lotus/extern/sector-storage/ffiwrapper/basicfs"
	sealing "github.com/filecoin-project/lotus/extern/storage-sealing"
	"github.com/filecoin-project/specs-storage/storage"
	"github.com/froghub-io/filecoin-sealer-recover/export"
	"github.com/mitchellh/go-homedir"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"time"
)

var RecoverCmd = &cli.Command{
	Name:  "recover",
	Usage: "Recovery sector tools",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "sectors-recovery-metadata",
			Usage: "specify the metadata file for the sectors recovery",
		},
		&cli.IntSliceFlag{
			Name:     "sector",
			Usage:    "Specify which sectors in the metadata file need to be recovered. Such as: 0",
			Required: true,
		},
		&cli.UintFlag{
			Name:  "parallel",
			Usage: "Number of parallel P1",
			Value: 1,
		},
		&cli.StringFlag{
			Name:  "sealing-result",
			Value: "~/sector",
			Usage: "Recover sector result path",
		},
		&cli.StringFlag{
			Name:  "sealing-temp",
			Value: "~/temp",
			Usage: "Temporarily generated during sector file",
		},
	},
	Action: func(cctx *cli.Context) error {
		log.Info("Start sealer recovery!")

		ctx := cliutil.DaemonContext(cctx)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		pssb := cctx.String("sectors-recovery-metadata")
		if pssb == "" {
			return xerrors.Errorf("Undefined sectors metadata")
		}

		log.Infof("Importing sectors metadata for %s", pssb)

		rp, err := migrateRecoverMeta(ctx, pssb)
		if err != nil {
			return xerrors.Errorf("migrating sectors metadata: %w", err)
		}

		runSectors := cctx.IntSlice("sector")
		sectorInfos := make(export.SectorInfos, 0)
		for _, sectorInfo := range rp.SectorInfos {
			for _, num := range runSectors {
				if uint64(num) == uint64(sectorInfo.SectorNumber) {
					sectorInfos = append(sectorInfos, sectorInfo)
				}
			}
		}
		rp.SectorInfos = sectorInfos

		if err = RecoverSealedFile(ctx, rp, cctx.Uint("parallel"), cctx.String("sealing-result"), cctx.String("sealing-temp")); err != nil {
			return err
		}
		log.Info("Complete recovery sealed!")
		return nil
	},
}

func migrateRecoverMeta(ctx context.Context, metadata string) (export.RecoveryParams, error) {
	metadata, err := homedir.Expand(metadata)
	if err != nil {
		return export.RecoveryParams{}, xerrors.Errorf("expanding sectors recovery dir: %w", err)
	}

	b, err := ioutil.ReadFile(metadata)
	if err != nil {
		return export.RecoveryParams{}, xerrors.Errorf("reading sectors recovery metadata: %w", err)
	}

	rp := export.RecoveryParams{}
	if err := json.Unmarshal(b, &rp); err != nil {
		return export.RecoveryParams{}, xerrors.Errorf("unmarshaling sectors recovery metadata: %w", err)
	}

	return rp, nil
}

func RecoverSealedFile(ctx context.Context, rp export.RecoveryParams, parallel uint, sealingResult string, sealingTemp string) error {
	actorID, err := address.IDFromAddress(rp.Miner)
	if err != nil {
		return xerrors.Errorf("Getting IDFromAddress err:", err)
	}

	wg := &sync.WaitGroup{}
	limiter := make(chan bool, parallel)
	var p1LastTaskTime time.Time
	for _, sector := range rp.SectorInfos {
		wg.Add(1)
		limiter <- true
		go func(sector *export.SectorInfo) {
			defer func() {
				wg.Done()
				<-limiter
			}()

			//Control PC1 running interval
			for {
				if time.Now().Add(-time.Minute * 10).After(p1LastTaskTime) {
					break
				}
				<-time.After(p1LastTaskTime.Sub(time.Now()))
			}
			p1LastTaskTime = time.Now()

			sdir, err := homedir.Expand(sealingTemp)
			if err != nil {
				log.Errorf("Sector (%d) ,expands the path error: %v", sector, err)
			}
			mkdirAll(sdir)
			tempDir, err := ioutil.TempDir(sdir, fmt.Sprintf("recover-%d", sector))
			if err != nil {
				log.Errorf("Sector (%d) ,creates a new temporary directory error: %v", sector, err)
			}
			if err := os.MkdirAll(tempDir, 0775); err != nil {
				log.Errorf("Sector (%d) ,creates a directory named path error: %v", sector, err)
			}
			sb, err := ffiwrapper.New(&basicfs.Provider{
				Root: tempDir,
			})
			if err != nil {
				log.Errorf("Sector (%d) ,new ffi Sealer error: %v", sector, err)
			}

			sid := storage.SectorRef{
				ID: abi.SectorID{
					Miner:  abi.ActorID(actorID),
					Number: sector.SectorNumber,
				},
				ProofType: sector.SealProof,
			}

			log.Infof("Start recover sector(%d,%d), registeredSealProof: %d, ticket: %x", actorID, sector, sector.SealProof, sector.Ticket)

			log.Infof("Start running AP, sector (%d)", sector)
			pi, err := sb.AddPiece(context.TODO(), sid, nil, abi.PaddedPieceSize(rp.SectorSize).Unpadded(), sealing.NewNullReader(abi.UnpaddedPieceSize(rp.SectorSize)))
			if err != nil {
				log.Errorf("Sector (%d) ,running AP  error: %v", sector, err)
			}
			var pieces []abi.PieceInfo
			pieces = append(pieces, pi)
			log.Infof("Complete AP, sector (%d)", sector)

			log.Infof("Start running PreCommit1, sector (%d)", sector)
			pc1o, err := sb.SealPreCommit1(context.TODO(), sid, abi.SealRandomness(sector.Ticket), []abi.PieceInfo{pi})
			if err != nil {
				log.Errorf("Sector (%d) , running PreCommit1  error: %v", sector, err)
			}
			log.Infof("Complete PreCommit1, sector (%d)", sector)

			err = sealPreCommit2AndCheck(ctx, sb, sid, pc1o, sector.SealedCID.String())
			if err != nil {
				log.Errorf("Sector (%d) , running PreCommit2  error: %v", sector, err)
			}

			err = MoveStorage(ctx, sid, tempDir, sealingResult)
			if err != nil {
				log.Errorf("Sector (%d) , running MoveStorage  error: %v", sector, err)
			}

			log.Infof("Complete sector (%d)", sector)
		}(sector)
	}
	wg.Wait()

	return nil
}

var pc2Lock sync.Mutex

func sealPreCommit2AndCheck(ctx context.Context, sb *ffiwrapper.Sealer, sid storage.SectorRef, phase1Out storage.PreCommit1Out, sealedCID string) error {
	pc2Lock.Lock()
	log.Infof("Start running PreCommit2, sector (%d)", sid.ID)

	cids, err := sb.SealPreCommit2(ctx, sid, phase1Out)
	if err != nil {
		pc2Lock.Unlock()
		return err
	}
	pc2Lock.Unlock()
	log.Infof("Complete PreCommit2, sector (%d)", sid.ID)

	//check CID with chain
	if sealedCID != cids.Sealed.String() {
		return xerrors.Errorf("sealed cid mismatching!!! (sealedCID: %v, newSealedCID: %v)", sealedCID, cids.Sealed.String())
	}
	return nil
}

func MoveStorage(ctx context.Context, sector storage.SectorRef, tempDir string, sealingResult string) error {
	//del unseal
	if err := os.RemoveAll(tempDir + "/unsealed"); err != nil {
		return xerrors.Errorf("SectorID: %d, del unseal error：%s", sector.ID, err)
	}
	sectorNum := "s-t0" + sector.ID.Miner.String() + "-" + sector.ID.Number.String()

	//del layer
	files, _ := ioutil.ReadDir(tempDir + "/cache/" + sectorNum)
	for _, f := range files {
		if strings.Contains(f.Name(), "layer") || strings.Contains(f.Name(), "tree-c") || strings.Contains(f.Name(), "tree-d") {
			if err := os.RemoveAll(tempDir + "/cache/" + sectorNum + "/" + f.Name()); err != nil {
				return xerrors.Errorf("SectorID: %d, del layer error：%s", sector.ID, err)
			}
		}
	}

	//move to storage
	mkdirAll(sealingResult)
	mkdirAll(sealingResult + "/cache")
	mkdirAll(sealingResult + "/sealed")
	if err := move(tempDir+"/cache/"+sectorNum, sealingResult+"/cache/"+sectorNum); err != nil {
		// return xerrors.Errorf("SectorID: %d, move cache error：%s", sector.ID, err)
		// change the output to warn info since this will no impact the result
		log.Warn("can move sector to your sealingResult, reason: ", err)
		return nil
	}
	if err := move(tempDir+"/sealed/"+sectorNum, sealingResult+"/sealed/"+sectorNum); err != nil {
		return xerrors.Errorf("SectorID: %d, move sealed error：%s", sector.ID, err)
	}

	return nil
}