// Package housekeeper contains the service doing all required regular tasks
//
// - Update known validators
// - Updating proposer duties
// - Saving metrics
// - Deleting old bids
// - ...
package housekeeper

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/flashbots/go-boost-utils/types"
	"github.com/flashbots/mev-boost-relay/beaconclient"
	"github.com/flashbots/mev-boost-relay/common"
	"github.com/flashbots/mev-boost-relay/datastore"
	"github.com/sirupsen/logrus"
	uberatomic "go.uber.org/atomic"
)

type HousekeeperOpts struct {
	Log          *logrus.Entry
	Redis        *datastore.RedisCache
	BeaconClient beaconclient.IMultiBeaconClient
}

type Housekeeper struct {
	opts *HousekeeperOpts
	log  *logrus.Entry

	redis        *datastore.RedisCache
	beaconClient beaconclient.IMultiBeaconClient

	isStarted                uberatomic.Bool
	isUpdatingProposerDuties uberatomic.Bool
	proposerDutiesSlot       uint64

	headSlot uberatomic.Uint64

	proposersAlreadySaved map[string]bool // to avoid repeating redis writes
}

var ErrServerAlreadyStarted = errors.New("server was already started")

func NewHousekeeper(opts *HousekeeperOpts) *Housekeeper {
	server := &Housekeeper{
		opts:                  opts,
		log:                   opts.Log,
		redis:                 opts.Redis,
		beaconClient:          opts.BeaconClient,
		proposersAlreadySaved: make(map[string]bool),
	}

	return server
}

// Start starts the housekeeper service, blocking
func (hk *Housekeeper) Start() (err error) {
	defer hk.isStarted.Store(false)
	if hk.isStarted.Swap(true) {
		return ErrServerAlreadyStarted
	}

	// Get best beacon-node status by head slot, process current slot and start slot updates
	bestSyncStatus, err := hk.beaconClient.BestSyncStatus()
	if err != nil {
		return err
	}

	// Start the periodic task loops
	go hk.periodicTaskUpdateKnownValidators()
	go hk.periodicTaskLogNumRegisteredValidators()
	go hk.periodicTaskUpdateBuilderStatusInRedis()

	// Process the current slot
	headSlot := bestSyncStatus.HeadSlot
	hk.processNewSlot(headSlot)

	// Start regular slot updates
	c := make(chan beaconclient.HeadEventData)
	hk.beaconClient.SubscribeToHeadEvents(c)
	for {
		headEvent := <-c
		hk.processNewSlot(headEvent.Slot)
	}
}

func (hk *Housekeeper) periodicTaskLogNumRegisteredValidators() {
	for {
		numRegisteredValidators, err := hk.redis.NumRegisteredValidators()
		if err == nil {
			hk.log.WithField("numRegisteredValidators", numRegisteredValidators).Infof("registered validators: %d", numRegisteredValidators)
		} else {
			hk.log.WithError(err).Error("failed to get number of registered validators")
		}
		time.Sleep(common.DurationPerEpoch / 2)
	}
}

func (hk *Housekeeper) periodicTaskUpdateKnownValidators() {
	for {
		hk.log.Debug("periodicTaskUpdateKnownValidators start")
		hk.updateKnownValidators()
		hk.log.Debug("periodicTaskUpdateKnownValidators done")

		// Wait half an epoch
		time.Sleep(common.DurationPerEpoch / 2)
	}
}

func (hk *Housekeeper) periodicTaskUpdateBuilderStatusInRedis() {
	for {
		// builders, err := hk.da
		time.Sleep(common.DurationPerEpoch / 2)
	}
}

func (hk *Housekeeper) processNewSlot(headSlot uint64) {
	prevHeadSlot := hk.headSlot.Load()
	if headSlot <= prevHeadSlot {
		return
	}

	log := hk.log.WithFields(logrus.Fields{
		"headSlot":     headSlot,
		"prevHeadSlot": prevHeadSlot,
	})

	if prevHeadSlot > 0 {
		for s := prevHeadSlot + 1; s < headSlot; s++ {
			log.WithField("missedSlot", s).Warnf("missed slot: %d", s)
		}
	}

	// Update proposer duties
	go hk.updateProposerDuties(headSlot)
	go func() {
		err := hk.redis.SetStats(datastore.RedisStatsFieldLatestSlot, headSlot)
		if err != nil {
			log.WithError(err).Error("failed to set stats")
		}
	}()

	hk.headSlot.Store(headSlot)
	currentEpoch := headSlot / uint64(common.SlotsPerEpoch)
	log.WithFields(logrus.Fields{
		"epoch":              currentEpoch,
		"slotStartNextEpoch": (currentEpoch + 1) * uint64(common.SlotsPerEpoch),
	}).Infof("updated headSlot to %d", headSlot)
}

func (hk *Housekeeper) updateKnownValidators() {
	// Query beacon node for known validators
	hk.log.Debug("Querying validators from beacon node... (this may take a while)")
	timeStartFetching := time.Now()
	validators, err := hk.beaconClient.FetchValidators(hk.headSlot.Load() - 1) // -1 to avoid "Invalid state ID: requested slot number is higher than head slot number" with multiple BNs
	if err != nil {
		hk.log.WithError(err).Error("failed to fetch validators from all beacon nodes")
		return
	}

	numValidators := len(validators)
	log := hk.log.WithField("numKnownValidators", numValidators)
	log.WithField("durationFetchValidators", time.Since(timeStartFetching).Seconds()).Infof("received validators from beacon-node")

	// Store total number of validators
	err = hk.redis.SetStats("validators_known_total", fmt.Sprint(numValidators))
	if err != nil {
		log.WithError(err).WithField(
			"field", "validators_known_total",
		).Error("failed to set status")
	}

	// Update Redis with validators
	log.Debug("Writing to Redis...")
	timeStartWriting := time.Now()

	printCounter := len(hk.proposersAlreadySaved) == 0 // only on first round
	i := 0
	newValidators := 0
	for _, validator := range validators {
		i++
		if printCounter && i%10000 == 0 {
			hk.log.Debugf("writing to redis: %d / %d", i, numValidators)
		}

		// avoid resaving
		if hk.proposersAlreadySaved[validator.Validator.Pubkey] {
			continue
		}

		err := hk.redis.SetKnownValidatorNX(types.PubkeyHex(validator.Validator.Pubkey), validator.Index)
		if err != nil {
			log.WithError(err).WithField("pubkey", validator.Validator.Pubkey).Error("failed to set known validator in Redis")
		} else {
			hk.proposersAlreadySaved[validator.Validator.Pubkey] = true
			newValidators++
		}
	}

	log.WithFields(logrus.Fields{
		"durationRedisWrite": time.Since(timeStartWriting).Seconds(),
		"newValidators":      newValidators,
	}).Info("updateKnownValidators done")
}

func (hk *Housekeeper) updateProposerDuties(headSlot uint64) {
	// Should only happen once at a time
	if hk.isUpdatingProposerDuties.Swap(true) {
		return
	}
	defer hk.isUpdatingProposerDuties.Store(false)

	if headSlot%uint64(common.SlotsPerEpoch/2) != 0 && headSlot-hk.proposerDutiesSlot < uint64(common.SlotsPerEpoch/2) {
		return
	}

	epoch := headSlot / uint64(common.SlotsPerEpoch)

	log := hk.log.WithFields(logrus.Fields{
		"epochFrom": epoch,
		"epochTo":   epoch + 1,
	})
	log.Debug("updating proposer duties...")

	// Query current epoch
	r, err := hk.beaconClient.GetProposerDuties(epoch)
	if err != nil {
		log.WithError(err).Error("failed to get proposer duties for all beacon nodes")
		return
	}

	entries := r.Data

	// Query next epoch
	r2, err := hk.beaconClient.GetProposerDuties(epoch + 1)
	if r2 != nil {
		entries = append(entries, r2.Data...)
	} else {
		log.WithError(err).Error("failed to get proposer duties for next epoch for all beacon nodes")
	}

	// Validator registrations are queried in parallel, and this is the result struct
	type result struct {
		val types.BuilderGetValidatorsResponseEntry
		err error
	}

	// Scatter requests to Redis to get validator registrations
	c := make(chan result, len(entries))
	for i := 0; i < cap(c); i++ {
		go func(duty beaconclient.ProposerDutiesResponseData) {
			reg, err := hk.redis.GetValidatorRegistration(types.NewPubkeyHex(duty.Pubkey))
			c <- result{types.BuilderGetValidatorsResponseEntry{
				Slot:  duty.Slot,
				Entry: reg,
			}, err}
		}(entries[i])
	}

	// Gather results
	proposerDuties := make([]types.BuilderGetValidatorsResponseEntry, 0)
	for i := 0; i < cap(c); i++ {
		res := <-c
		if res.err != nil {
			log.WithError(res.err).Error("error in loading validator registration from redis")
		} else if res.val.Entry != nil { // only if a known registration
			proposerDuties = append(proposerDuties, res.val)
		}
	}

	// Save duties to Redis
	err = hk.redis.SetProposerDuties(proposerDuties)
	if err != nil {
		log.WithError(err).Error("failed to set proposer duties")
		return
	}
	hk.proposerDutiesSlot = headSlot

	// Pretty-print
	_duties := make([]string, len(proposerDuties))
	for i, duty := range proposerDuties {
		_duties[i] = fmt.Sprint(duty.Slot)
	}
	sort.Strings(_duties)
	log.WithField("numDuties", len(_duties)).Infof("proposer duties updated: %s", strings.Join(_duties, ", "))
}
