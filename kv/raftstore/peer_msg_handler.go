package raftstore

import (
	"fmt"
	"time"

	"github.com/Connor1996/badger/y"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/message"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/runner"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/snap"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/util"
	"github.com/pingcap-incubator/tinykv/kv/worker"
	"github.com/pingcap-incubator/tinykv/log"
	"github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/metapb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/raft_cmdpb"
	rspb "github.com/pingcap-incubator/tinykv/proto/pkg/raft_serverpb"
	"github.com/pingcap-incubator/tinykv/scheduler/pkg/btree"
	"github.com/pingcap/errors"
)

type PeerTick int

const (
	PeerTickRaft             PeerTick = 0
	PeerTickRaftLogGC        PeerTick = 1
	PeerTickSplitRegionCheck PeerTick = 2
	PeerTickPdHeartbeat      PeerTick = 3
)

type peerMsgHandler struct {
	*peer
	applyCh chan []message.Msg
	ctx     *GlobalContext
}

func newPeerMsgHandler(peer *peer, applyCh chan []message.Msg, ctx *GlobalContext) *peerMsgHandler {
	return &peerMsgHandler{
		peer:    peer,
		applyCh: applyCh,
		ctx:     ctx,
	}
}

func (d *peerMsgHandler) HandleMsgs(msg message.Msg) {
	switch msg.Type {
	case message.MsgTypeRaftMessage:
		raftMsg := msg.Data.(*rspb.RaftMessage)
		if err := d.onRaftMsg(raftMsg); err != nil {
			log.Errorf("%s handle raft message error %v", d.peer.Tag, err)
		}
	case message.MsgTypeRaftCmd:
		raftCMD := msg.Data.(*message.MsgRaftCmd)
		d.proposeRaftCommand(raftCMD.Request, raftCMD.Callback)
	case message.MsgTypeTick:
		d.onTick()
	case message.MsgTypeApplyRes:
		res := msg.Data.(*MsgApplyRes)
		d.onApplyResult(res)
	case message.MsgTypeSplitRegion:
		split := msg.Data.(*message.MsgSplitRegion)
		log.Infof("%s on split with %v", d.peer.Tag, split.SplitKeys)
		d.onPrepareSplitRegion(split.RegionEpoch, split.SplitKeys, split.Callback)
	case message.MsgTypeRegionApproximateSize:
		d.onApproximateRegionSize(msg.Data.(uint64))
	case message.MsgTypeGcSnap:
		gcSnap := msg.Data.(*message.MsgGCSnap)
		d.onGCSnap(gcSnap.Snaps)
	case message.MsgTypeStart:
		d.startTicker()
	}
}

func (d *peerMsgHandler) onTick() {
	if d.stopped {
		return
	}
	d.ticker.tickClock()
	if d.ticker.isOnTick(PeerTickRaft) {
		d.onRaftBaseTick()
	}
	if d.ticker.isOnTick(PeerTickRaftLogGC) {
		d.onRaftGCLogTick()
	}
	if d.ticker.isOnTick(PeerTickPdHeartbeat) {
		d.onPDHeartbeatTick()
	}
	if d.ticker.isOnTick(PeerTickSplitRegionCheck) {
		d.onSplitRegionCheckTick()
	}
	d.ctx.tickDriverSender <- d.regionID()
}

func (d *peerMsgHandler) startTicker() {
	d.ticker = newTicker(d.regionID(), d.ctx.cfg)
	d.ctx.tickDriverSender <- d.regionID()
	d.ticker.schedule(PeerTickRaft)
	d.ticker.schedule(PeerTickRaftLogGC)
	d.ticker.schedule(PeerTickSplitRegionCheck)
	d.ticker.schedule(PeerTickPdHeartbeat)
}

func (d *peerMsgHandler) onGCSnap(snaps []snap.SnapKeyWithSending) {
	store := d.peer.Store()
	compactedIdx := store.truncatedIndex()
	compactedTerm := store.truncatedTerm()
	isApplyingSnap := store.IsApplyingSnapshot()
	for _, snapKeyWithSending := range snaps {
		key := snapKeyWithSending.SnapKey
		if snapKeyWithSending.IsSending {
			snap, err := d.ctx.snapMgr.GetSnapshotForSending(key)
			if err != nil {
				log.Errorf("%s failed to load snapshot for %s %v", d.tag(), key, err)
				continue
			}
			if key.Term < compactedTerm || key.Index < compactedIdx {
				log.Infof("%s snap file %s has been compacted, delete", d.tag(), key)
				d.ctx.snapMgr.DeleteSnapshot(key, snap, false)
			} else if fi, err1 := snap.Meta(); err1 == nil {
				modTime := fi.ModTime()
				if time.Since(modTime) > 4*time.Hour {
					log.Infof("%s snap file %s has been expired, delete", d.tag(), key)
					d.ctx.snapMgr.DeleteSnapshot(key, snap, false)
				}
			}
		} else if key.Term <= compactedTerm &&
			(key.Index < compactedIdx || (key.Index == compactedIdx && !isApplyingSnap)) {
			log.Infof("%s snap file %s has been applied, delete", d.tag(), key)
			a, err := d.ctx.snapMgr.GetSnapshotForApplying(key)
			if err != nil {
				log.Errorf("%s failed to load snapshot for %s %v", d.tag(), key, err)
				continue
			}
			d.ctx.snapMgr.DeleteSnapshot(key, a, false)
		}
	}
}

func (d *peerMsgHandler) HandleRaftReady() {
	if d.stopped {
		return
	}

	// Your Code Here (2B).
	// TODO: Delete Start
	msgs := make([]message.Msg, 0)
	if p := d.peer.TakeApplyProposals(); p != nil {
		msg := message.Msg{Type: message.MsgTypeApplyProposal, Data: p, RegionID: p.RegionId}
		msgs = append(msgs, msg)
	}
	applySnapResult, msgs := d.peer.HandleRaftReady(msgs, d.ctx.pdTaskSender, d.ctx.trans)
	if applySnapResult != nil {
		prevRegion := applySnapResult.PrevRegion
		region := applySnapResult.Region

		log.Infof("%s snapshot for region %s is applied", d.tag(), region)
		meta := d.ctx.storeMeta
		initialized := len(prevRegion.Peers) > 0
		if initialized {
			log.Infof("%s region changed from %s -> %s after applying snapshot", d.tag(), prevRegion, region)
			meta.regionRanges.Delete(&regionItem{region: prevRegion})
		}
		if oldRegion := meta.regionRanges.ReplaceOrInsert(&regionItem{region: region}); oldRegion != nil {
			panic(fmt.Sprintf("%s unexpected old region %+v, region %+v", d.tag(), oldRegion, region))
		}
		meta.regions[region.Id] = region
	}
	d.applyCh <- msgs
	// TODO: Delete End
}

func (d *peerMsgHandler) onRaftBaseTick() {
	if d.peer.PendingRemove {
		return
	}
	// When having pending snapshot, if election timeout is met, it can't pass
	// the pending conf change check because first index has been updated to
	// a value that is larger than last index.
	if d.peer.IsApplyingSnapshot() || d.peer.HasPendingSnapshot() {
		// need to check if snapshot is applied.
		d.ticker.schedule(PeerTickRaft)
		return
	}
	// TODO: make Tick returns bool to indicate if there is ready.
	d.peer.RaftGroup.Tick()
	d.ticker.schedule(PeerTickRaft)
}

func (d *peerMsgHandler) onApplyResult(res *MsgApplyRes) {
	// Your Code Here (2B).

	// TODO: Delete Start
	log.Debugf("%s async apply finished %v", d.tag(), res)
	// handle executing committed log results
	for _, result := range res.execResults {
		switch x := result.(type) {
		case *execResultChangePeer:
			d.onReadyChangePeer(x)
		case *execResultCompactLog:
			d.onReadyCompactLog(x.firstIndex, x.truncatedIndex)
		case *execResultSplitRegion:
			d.onReadySplitRegion(x.derived, x.regions)
		}
	}
	res.execResults = nil
	if d.stopped {
		return
	}

	diff := d.peer.SizeDiffHint + res.sizeDiffHint
	if diff > 0 {
		d.peer.SizeDiffHint = diff
	} else {
		d.peer.SizeDiffHint = 0
	}
	// TODO: Delete End
}

func (d *peerMsgHandler) onRaftMsg(msg *rspb.RaftMessage) error {
	log.Debugf("%s handle raft message %s from %d to %d",
		d.tag(), msg.GetMessage().GetMsgType(), msg.GetFromPeer().GetId(), msg.GetToPeer().GetId())
	if !d.validateRaftMessage(msg) {
		return nil
	}
	if d.peer.PendingRemove || d.stopped {
		return nil
	}
	if msg.GetIsTombstone() {
		// we receive a message tells us to remove self.
		d.handleGCPeerMsg(msg)
		return nil
	}
	if d.checkMessage(msg) {
		return nil
	}
	key, err := d.checkSnapshot(msg)
	if err != nil {
		return err
	}
	if key != nil {
		// If the snapshot file is not used again, then it's OK to
		// delete them here. If the snapshot file will be reused when
		// receiving, then it will fail to pass the check again, so
		// missing snapshot files should not be noticed.
		s, err1 := d.ctx.snapMgr.GetSnapshotForApplying(*key)
		if err1 != nil {
			return err1
		}
		d.ctx.snapMgr.DeleteSnapshot(*key, s, false)
		return nil
	}
	d.peer.insertPeerCache(msg.GetFromPeer())
	err = d.peer.Step(msg.GetMessage())
	if err != nil {
		return err
	}
	if d.peer.AnyNewPeerCatchUp(msg.FromPeer.Id) {
		d.peer.HeartbeatPd(d.ctx.pdTaskSender)
	}
	return nil
}

// return false means the message is invalid, and can be ignored.
func (d *peerMsgHandler) validateRaftMessage(msg *rspb.RaftMessage) bool {
	regionID := msg.GetRegionId()
	from := msg.GetFromPeer()
	to := msg.GetToPeer()
	log.Debugf("[region %d] handle raft message %s from %d to %d", regionID, msg, from.GetId(), to.GetId())
	if to.GetStoreId() != d.storeID() {
		log.Warnf("[region %d] store not match, to store id %d, mine %d, ignore it",
			regionID, to.GetStoreId(), d.storeID())
		return false
	}
	if msg.RegionEpoch == nil {
		log.Errorf("[region %d] missing epoch in raft message, ignore it", regionID)
		return false
	}
	return true
}

/// Checks if the message is sent to the correct peer.
///
/// Returns true means that the message can be dropped silently.
func (d *peerMsgHandler) checkMessage(msg *rspb.RaftMessage) bool {
	fromEpoch := msg.GetRegionEpoch()
	isVoteMsg := util.IsVoteMessage(msg.Message)
	fromStoreID := msg.FromPeer.GetStoreId()

	// Let's consider following cases with three nodes [1, 2, 3] and 1 is leader:
	// a. 1 removes 2, 2 may still send MsgAppendResponse to 1.
	//  We should ignore this stale message and let 2 remove itself after
	//  applying the ConfChange log.
	// b. 2 is isolated, 1 removes 2. When 2 rejoins the cluster, 2 will
	//  send stale MsgRequestVote to 1 and 3, at this time, we should tell 2 to gc itself.
	// c. 2 is isolated but can communicate with 3. 1 removes 3.
	//  2 will send stale MsgRequestVote to 3, 3 should ignore this message.
	// d. 2 is isolated but can communicate with 3. 1 removes 2, then adds 4, remove 3.
	//  2 will send stale MsgRequestVote to 3, 3 should tell 2 to gc itself.
	// e. 2 is isolated. 1 adds 4, 5, 6, removes 3, 1. Now assume 4 is leader.
	//  After 2 rejoins the cluster, 2 may send stale MsgRequestVote to 1 and 3,
	//  1 and 3 will ignore this message. Later 4 will send messages to 2 and 2 will
	//  rejoin the raft group again.
	// f. 2 is isolated. 1 adds 4, 5, 6, removes 3, 1. Now assume 4 is leader, and 4 removes 2.
	//  unlike case e, 2 will be stale forever.
	// TODO: for case f, if 2 is stale for a long time, 2 will communicate with pd and pd will
	// tell 2 is stale, so 2 can remove itself.
	region := d.peer.Region()
	if util.IsEpochStale(fromEpoch, region.RegionEpoch) && util.FindPeer(region, fromStoreID) == nil {
		// The message is stale and not in current region.
		handleStaleMsg(d.ctx.trans, msg, region.RegionEpoch, isVoteMsg)
		return true
	}
	target := msg.GetToPeer()
	if target.Id < d.peerID() {
		log.Infof("%s target peer ID %d is less than %d, msg maybe stale", d.tag(), target.Id, d.peerID())
		return true
	} else if target.Id > d.peerID() {
		if d.peer.MaybeDestroy() {
			log.Infof("%s is stale as received a larger peer %s, destroying", d.tag(), target)
			d.destroyPeer()
			d.ctx.router.sendStore(message.NewMsg(message.MsgTypeStoreRaftMessage, msg))
		}
		return true
	}
	return false
}

func handleStaleMsg(trans Transport, msg *rspb.RaftMessage, curEpoch *metapb.RegionEpoch,
	needGC bool) {
	regionID := msg.RegionId
	fromPeer := msg.FromPeer
	toPeer := msg.ToPeer
	msgType := msg.Message.GetMsgType()

	if !needGC {
		log.Infof("[region %d] raft message %s is stale, current %v ignore it",
			regionID, msgType, curEpoch)
		return
	}
	gcMsg := &rspb.RaftMessage{
		RegionId:    regionID,
		FromPeer:    fromPeer,
		ToPeer:      toPeer,
		RegionEpoch: curEpoch,
		IsTombstone: true,
	}
	if err := trans.Send(gcMsg); err != nil {
		log.Errorf("[region %d] send message failed %v", regionID, err)
	}
}

func (d *peerMsgHandler) handleGCPeerMsg(msg *rspb.RaftMessage) {
	fromEpoch := msg.RegionEpoch
	if !util.IsEpochStale(d.peer.Region().RegionEpoch, fromEpoch) {
		return
	}
	if !util.PeerEqual(d.peer.Meta, msg.ToPeer) {
		log.Infof("%s receive stale gc msg, ignore", d.tag())
		return
	}
	log.Infof("%s peer %s receives gc message, trying to remove", d.tag(), msg.ToPeer)
	if d.peer.MaybeDestroy() {
		d.destroyPeer()
	}
}

// Returns `None` if the `msg` doesn't contain a snapshot or it contains a snapshot which
// doesn't conflict with any other snapshots or regions. Otherwise a `snap.SnapKey` is returned.
func (d *peerMsgHandler) checkSnapshot(msg *rspb.RaftMessage) (*snap.SnapKey, error) {
	if msg.Message.Snapshot == nil {
		return nil, nil
	}
	regionID := msg.RegionId
	snapshot := msg.Message.Snapshot
	key := snap.SnapKeyFromRegionSnap(regionID, snapshot)
	snapData := new(rspb.RaftSnapshotData)
	err := snapData.Unmarshal(snapshot.Data)
	if err != nil {
		return nil, err
	}
	snapRegion := snapData.Region
	peerID := msg.ToPeer.Id
	var contains bool
	for _, peer := range snapRegion.Peers {
		if peer.Id == peerID {
			contains = true
			break
		}
	}
	if !contains {
		log.Infof("%s %s doesn't contains peer %d, skip", d.tag(), snapRegion, peerID)
		return &key, nil
	}
	meta := d.ctx.storeMeta
	if !util.RegionEqual(meta.regions[d.regionID()], d.region()) {
		if !d.peer.isInitialized() {
			log.Infof("%s stale delegate detected, skip", d.tag())
			return &key, nil
		} else {
			panic(fmt.Sprintf("%s meta corrupted %s != %s", d.tag(), meta.regions[d.regionID()], d.region()))
		}
	}

	existRegions := meta.getOverlapRegions(snapRegion)
	for _, existRegion := range existRegions {
		if existRegion.GetId() == snapRegion.GetId() {
			continue
		}
		log.Infof("%s region overlapped %s %s", d.tag(), existRegion, snapRegion)
		return &key, nil
	}

	// check if snapshot file exists.
	_, err = d.ctx.snapMgr.GetSnapshotForApplying(key)
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (d *peerMsgHandler) destroyPeer() {
	log.Infof("%s starts destroy", d.tag())
	regionID := d.regionID()
	// We can't destroy a peer which is applying snapshot.
	y.Assert(!d.peer.IsApplyingSnapshot())
	meta := d.ctx.storeMeta
	isInitialized := d.peer.isInitialized()
	if err := d.peer.Destroy(d.ctx.engine, false); err != nil {
		// If not panic here, the peer will be recreated in the next restart,
		// then it will be gc again. But if some overlap region is created
		// before restarting, the gc action will delete the overlap region's
		// data too.
		panic(fmt.Sprintf("%s destroy peer %v", d.tag(), err))
	}
	d.ctx.router.close(regionID)
	d.stop()
	if isInitialized && meta.regionRanges.Delete(&regionItem{region: d.region()}) == nil {
		panic(d.tag() + " meta corruption detected")
	}
	if _, ok := meta.regions[regionID]; !ok {
		panic(d.tag() + " meta corruption detected")
	}
	delete(meta.regions, regionID)
}

func (d *peerMsgHandler) onReadyChangePeer(cp *execResultChangePeer) {
	// Your Code Here (3B).
	// TODO: Delete Start
	changeType := cp.confChange.ChangeType
	d.peer.RaftGroup.ApplyConfChange(*cp.confChange)
	if cp.confChange.NodeId == 0 {
		// Apply failed, skip.
		return
	}
	d.ctx.storeMeta.setRegion(cp.region, d.peer)
	peerID := cp.peer.Id
	switch changeType {
	case eraftpb.ConfChangeType_AddNode:
		// Add this peer to cache and heartbeats.
		now := time.Now()
		d.peer.PeerHeartbeats[peerID] = now
		if d.peer.IsLeader() {
			d.peer.PeersStartPendingTime[peerID] = now
		}
		d.peer.insertPeerCache(cp.peer)
	case eraftpb.ConfChangeType_RemoveNode:
		// Remove this peer from cache.
		delete(d.peer.PeerHeartbeats, peerID)
		if d.peer.IsLeader() {
			delete(d.peer.PeersStartPendingTime, peerID)
		}
		d.peer.removePeerCache(peerID)
	}

	// In pattern matching above, if the peer is the leader,
	// it will push the change peer into `peers_start_pending_time`
	// without checking if it is duplicated. We move `heartbeat_pd` here
	// to utilize `collect_pending_peers` in `heartbeat_pd` to avoid
	// adding the redundant peer.
	if d.peer.IsLeader() {
		// Notify pd immediately.
		log.Infof("%s notify pd with change peer region %s", d.tag(), d.region())
		d.peer.HeartbeatPd(d.ctx.pdTaskSender)
	}
	myPeerID := d.peerID()

	// We only care remove itself now.
	if changeType == eraftpb.ConfChangeType_RemoveNode && cp.peer.StoreId == d.storeID() {
		if myPeerID == peerID {
			d.destroyPeer()
		} else {
			panic(fmt.Sprintf("%s trying to remove unknown peer %s", d.tag(), cp.peer))
		}
	}
	// TODO: Delete End
}

func (d *peerMsgHandler) onReadyCompactLog(firstIndex uint64, truncatedIndex uint64) {
	raftLogGCTask := &runner.RaftLogGCTask{
		RaftEngine: d.ctx.engine.Raft,
		RegionID:   d.regionID(),
		StartIdx:   d.peer.LastCompactedIdx,
		EndIdx:     truncatedIndex + 1,
	}
	d.peer.LastCompactedIdx = raftLogGCTask.EndIdx
	d.ctx.raftLogGCTaskSender <- worker.Task{
		Tp:   worker.TaskTypeRaftLogGC,
		Data: raftLogGCTask,
	}
}

func (d *peerMsgHandler) onReadySplitRegion(derived *metapb.Region, regions []*metapb.Region) {
	// Your Code Here (3B).
	// TODO: Delete Start
	meta := d.ctx.storeMeta
	regionID := derived.Id
	meta.setRegion(derived, d.peer)
	d.peer.SizeDiffHint = 0
	isLeader := d.peer.IsLeader()
	if isLeader {
		d.peer.HeartbeatPd(d.ctx.pdTaskSender)
		// Notify pd immediately to let it update the region meta.
		log.Infof("%s notify pd with split count %d", d.tag(), len(regions))
	}

	if meta.regionRanges.Delete(&regionItem{region: regions[0]}) == nil {
		panic(d.tag() + " original region should exist")
	}
	// It's not correct anymore, so set it to None to let split checker update it.
	d.peer.ApproximateSize = nil

	for _, newRegion := range regions {
		newRegionID := newRegion.Id
		notExist := meta.regionRanges.ReplaceOrInsert(&regionItem{region: newRegion})
		if notExist != nil {
			panic(fmt.Sprintf("%v %v newregion:%v, region:%v", d.tag(), notExist.(*regionItem).region, newRegion, regions[0]))
		}
		if newRegionID == regionID {
			continue
		}

		// Insert new regions and validation
		log.Infof("[region %d] inserts new region %s", regionID, newRegion)
		if r, ok := meta.regions[newRegionID]; ok {
			// Suppose a new node is added by conf change and the snapshot comes slowly.
			// Then, the region splits and the first vote message comes to the new node
			// before the old snapshot, which will create an uninitialized peer on the
			// store. After that, the old snapshot comes, followed with the last split
			// proposal. After it's applied, the uninitialized peer will be met.
			// We can remove this uninitialized peer directly.
			if len(r.Peers) > 0 {
				panic(fmt.Sprintf("[region %d] duplicated region %s for split region %s",
					newRegionID, r, newRegion))
			}
			d.ctx.router.close(newRegionID)
		}

		newPeer, err := createPeer(d.ctx.store.Id, d.ctx.cfg, d.ctx.regionTaskSender, d.ctx.engine, newRegion)
		if err != nil {
			// peer information is already written into db, can't recover.
			// there is probably a bug.
			panic(fmt.Sprintf("create new split region %s error %v", newRegion, err))
		}
		metaPeer := newPeer.Meta

		for _, p := range newRegion.GetPeers() {
			newPeer.insertPeerCache(p)
		}

		// New peer derive write flow from parent region,
		// this will be used by balance write flow.
		campaigned := newPeer.MaybeCampaign(isLeader)

		if isLeader {
			// The new peer is likely to become leader, send a heartbeat immediately to reduce
			// client query miss.
			newPeer.HeartbeatPd(d.ctx.pdTaskSender)
		}

		meta.regions[newRegionID] = newRegion
		d.ctx.router.register(newPeer)
		_ = d.ctx.router.send(newRegionID, message.NewPeerMsg(message.MsgTypeStart, newRegionID, nil))
		if !campaigned {
			for i, msg := range meta.pendingVotes {
				if util.PeerEqual(msg.ToPeer, metaPeer) {
					meta.pendingVotes = append(meta.pendingVotes[:i], meta.pendingVotes[i+1:]...)
					_ = d.ctx.router.send(newRegionID, message.NewPeerMsg(message.MsgTypeRaftMessage, newRegionID, msg))
					break
				}
			}
		}
	}
	// TODO: Delete End
}

func (d *peerMsgHandler) preProposeRaftCommand(req *raft_cmdpb.RaftCmdRequest) error {
	// Check store_id, make sure that the msg is dispatched to the right place.
	if err := util.CheckStoreID(req, d.storeID()); err != nil {
		return err
	}

	// Check whether the store has the right peer to handle the request.
	regionID := d.regionID()
	leaderID := d.peer.LeaderId()
	if !d.peer.IsLeader() {
		leader := d.peer.getPeerFromCache(leaderID)
		return &util.ErrNotLeader{RegionId: regionID, Leader: leader}
	}
	// peer_id must be the same as peer's.
	if err := util.CheckPeerID(req, d.peerID()); err != nil {
		return err
	}
	// Check whether the term is stale.
	if err := util.CheckTerm(req, d.peer.Term()); err != nil {
		return err
	}
	err := util.CheckRegionEpoch(req, d.region(), true)
	if errEpochNotMatching, ok := err.(*util.ErrEpochNotMatch); ok {
		// Attach the region which might be split from the current region. But it doesn't
		// matter if the region is not split from the current region. If the region meta
		// received by the TiKV driver is newer than the meta cached in the driver, the meta is
		// updated.
		siblingRegion := d.findSiblingRegion()
		if siblingRegion != nil {
			errEpochNotMatching.Regions = append(errEpochNotMatching.Regions, siblingRegion)
		}
		return errEpochNotMatching
	}
	return err
}

func (d *peerMsgHandler) proposeRaftCommand(msg *raft_cmdpb.RaftCmdRequest, cb *message.Callback) {
	// Your Code Here (2B).
	// TODO: Delete Start
	err := d.preProposeRaftCommand(msg)
	if err != nil {
		cb.Done(ErrResp(err))
		return
	}

	if d.peer.PendingRemove {
		NotifyReqRegionRemoved(d.regionID(), cb)
		return
	}

	// Note:
	// The peer that is being checked is a leader. It might step down to be a follower later. It
	// doesn't matter whether the peer is a leader or not. If it's not a leader, the proposing
	// command log entry can't be committed.
	resp := &raft_cmdpb.RaftCmdResponse{}
	BindRespTerm(resp, d.peer.Term())
	d.peer.Propose(d.ctx.engine.Kv, d.ctx.cfg, cb, msg, resp)
	// TODO: Delete End
}

func (d *peerMsgHandler) findSiblingRegion() (result *metapb.Region) {
	meta := d.ctx.storeMeta
	item := &regionItem{region: d.region()}
	meta.regionRanges.AscendGreaterOrEqual(item, func(i btree.Item) bool {
		result = i.(*regionItem).region
		return true
	})
	return
}

func (d *peerMsgHandler) onRaftGCLogTick() {
	d.ticker.schedule(PeerTickRaftLogGC)
	if !d.peer.IsLeader() {
		return
	}

	appliedIdx := d.peer.Store().AppliedIndex()
	firstIdx, _ := d.peer.Store().FirstIndex()
	var compactIdx uint64
	if appliedIdx > firstIdx && appliedIdx-firstIdx >= d.ctx.cfg.RaftLogGcCountLimit {
		compactIdx = appliedIdx
	} else {
		return
	}

	// Have no idea why subtract 1 here, but original code did this by magic.
	y.Assert(compactIdx > 0)
	compactIdx -= 1
	if compactIdx < firstIdx {
		// In case compact_idx == first_idx before subtraction.
		return
	}

	term, err := d.peer.RaftGroup.Raft.RaftLog.Term(compactIdx)
	if err != nil {
		log.Fatalf("appliedIdx: %d, firstIdx: %d, compactIdx: %d", appliedIdx, firstIdx, compactIdx)
		panic(err)
	}

	// Create a compact log request and notify directly.
	regionID := d.regionID()
	request := newCompactLogRequest(regionID, d.peer.Meta, compactIdx, term)
	d.proposeRaftCommand(request, nil)
}

func (d *peerMsgHandler) onSplitRegionCheckTick() {
	d.ticker.schedule(PeerTickSplitRegionCheck)
	// To avoid frequent scan, we only add new scan tasks if all previous tasks
	// have finished.
	if len(d.ctx.splitCheckTaskSender) > 0 {
		return
	}

	if !d.peer.IsLeader() {
		return
	}
	if d.peer.ApproximateSize != nil && d.peer.SizeDiffHint < d.ctx.cfg.RegionSplitSize/8 {
		return
	}
	d.ctx.splitCheckTaskSender <- worker.Task{
		Tp: worker.TaskTypeSplitCheck,
		Data: &runner.SplitCheckTask{
			Region: d.region(),
		},
	}
	d.peer.SizeDiffHint = 0
}

func (d *peerMsgHandler) onPrepareSplitRegion(regionEpoch *metapb.RegionEpoch, splitKeys [][]byte, cb *message.Callback) {
	if err := d.validateSplitRegion(regionEpoch, splitKeys); err != nil {
		cb.Done(ErrResp(err))
		return
	}
	region := d.region()
	d.ctx.pdTaskSender <- worker.Task{
		Tp: worker.TaskTypePDAskBatchSplit,
		Data: &runner.PdAskBatchSplitTask{
			Region:    region,
			SplitKeys: splitKeys,
			Peer:      d.peer.Meta,
			Callback:  cb,
		},
	}
}

func (d *peerMsgHandler) validateSplitRegion(epoch *metapb.RegionEpoch, splitKeys [][]byte) error {
	if len(splitKeys) == 0 {
		err := errors.Errorf("%s no split key is specified", d.tag())
		log.Error(err)
		return err
	}
	for _, key := range splitKeys {
		if len(key) == 0 {
			err := errors.Errorf("%s split key should not be empty", d.tag())
			log.Error(err)
			return err
		}
	}
	if !d.peer.IsLeader() {
		// region on this store is no longer leader, skipped.
		log.Infof("%s not leader, skip", d.tag())
		return &util.ErrNotLeader{
			RegionId: d.regionID(),
			Leader:   d.peer.getPeerFromCache(d.peer.LeaderId()),
		}
	}

	region := d.region()
	latestEpoch := region.GetRegionEpoch()

	// This is a little difference for `check_region_epoch` in region split case.
	// Here we just need to check `version` because `conf_ver` will be update
	// to the latest value of the peer, and then send to PD.
	if latestEpoch.Version != epoch.Version {
		log.Infof("%s epoch changed, retry later, prev_epoch: %s, epoch %s",
			d.tag(), latestEpoch, epoch)
		return &util.ErrEpochNotMatch{
			Message: fmt.Sprintf("%s epoch changed %s != %s, retry later", d.tag(), latestEpoch, epoch),
			Regions: []*metapb.Region{region},
		}
	}
	return nil
}

func (d *peerMsgHandler) onApproximateRegionSize(size uint64) {
	d.peer.ApproximateSize = &size
}

func (d *peerMsgHandler) onPDHeartbeatTick() {
	d.ticker.schedule(PeerTickPdHeartbeat)
	d.peer.CheckPeers()

	if !d.peer.IsLeader() {
		return
	}
	d.peer.HeartbeatPd(d.ctx.pdTaskSender)
}

func newAdminRequest(regionID uint64, peer *metapb.Peer) *raft_cmdpb.RaftCmdRequest {
	return &raft_cmdpb.RaftCmdRequest{
		Header: &raft_cmdpb.RaftRequestHeader{
			RegionId: regionID,
			Peer:     peer,
		},
	}
}

func newCompactLogRequest(regionID uint64, peer *metapb.Peer, compactIndex, compactTerm uint64) *raft_cmdpb.RaftCmdRequest {
	req := newAdminRequest(regionID, peer)
	req.AdminRequest = &raft_cmdpb.AdminRequest{
		CmdType: raft_cmdpb.AdminCmdType_CompactLog,
		CompactLog: &raft_cmdpb.CompactLogRequest{
			CompactIndex: compactIndex,
			CompactTerm:  compactTerm,
		},
	}
	return req
}
