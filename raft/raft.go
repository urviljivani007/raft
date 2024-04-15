package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"6.824/labgob"
	"6.824/labrpc"
)

//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in Lab 3 you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh; at that point you can add fields to
// ApplyMsg, but set CommandValid to false for these other uses.
//
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	SnapshotValid bool
	SnapshotIndex int
	SnapshotTerm int
	Snapshot []byte
}

const (
	LEADER    = 0
	CANDIDATE = 1
	FOLLOWER  = 2

	HEARTBEAT            = 200 * time.Millisecond
	ELECTION_TIMEOUT_MIN = 300
	ELECTION_TIMEOUT_MAX = 500
)

type Log struct {
	Term    int
	Command interface{}
}

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	// Your data here (2A, 2B, 2C).
	// Look at the paper's Figure 2 for a description of what
	// state a Raft server must maintain.
	state int

	// persistent state on all servers
	currentTerm int
	votedFor    int
	log         []Log

	// volatile state on all servers
	commitIndex int
	lastApplied int

	// volatile state on leaders
	nextIndex  []int
	matchIndex []int

	// additional fields that are helpful for implementation
	electionTimeout time.Duration
	lastHeard       time.Time  // last time heard from the leader
	voteCount       int        // how many votes does a candidate get from an election
	applyCond       *sync.Cond // used to kick the applier goroutine
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {

	var term int
	var isleader bool
	// Your code here (2A).
	rf.mu.Lock()
	term = rf.currentTerm
	isleader = rf.state == LEADER
	rf.mu.Unlock()
	return term, isleader
}

// periodically check if there are logs to be applied
// i.e. tf.commitIndex > rf.lastApplied
func (rf *Raft) applier(applyChan chan ApplyMsg) {
	for {
		if rf.killed() {
			return
		}

		rf.mu.Lock()
		for rf.commitIndex <= rf.lastApplied {
			rf.applyCond.Wait()
		}
		for rf.lastApplied < rf.commitIndex {
			rf.lastApplied++
			applyChan <- ApplyMsg{CommandValid: true, Command: rf.log[rf.lastApplied].Command, CommandIndex: rf.lastApplied}
		}
		rf.mu.Unlock()
	}
}

func (rf *Raft) resetTimer() {
	rf.mu.Lock()
	interval := rand.Intn(ELECTION_TIMEOUT_MAX-ELECTION_TIMEOUT_MIN) + ELECTION_TIMEOUT_MIN
	rf.electionTimeout = time.Duration(interval) * time.Millisecond
	rf.lastHeard = time.Now()
	rf.mu.Unlock()
}

func (rf *Raft) startHeartBeat() {
	for {
		if rf.killed() {
			return
		}

		rf.mu.Lock()
		if rf.state != LEADER {
			rf.mu.Unlock()
			return
		}
		term := rf.currentTerm
		prevLogIndex := len(rf.log) - 1
		prevLogTerm := rf.log[prevLogIndex].Term
		leaderCommit := rf.commitIndex
		rf.mu.Unlock()

		peerNum := len(rf.peers)
		for i := 0; i < peerNum; i++ {
			if i == rf.me {
				continue
			}
			args := AppendEntriesArgs{term, rf.me, prevLogIndex, prevLogTerm, []Log{}, leaderCommit}
			reply := AppendEntriesReply{}
			go rf.sendAppendEntries(i, &args, &reply)
		}
		time.Sleep(HEARTBEAT)
	}
}

func (rf *Raft) leader() {
	// comes to power, initialize fields
	rf.mu.Lock()
	rf.state = LEADER
	rf.votedFor = -1
	peerNum := len(rf.peers)
	for i := 0; i < peerNum; i++ {
		rf.nextIndex[i] = len(rf.log) // leader last log index + 1
		rf.matchIndex[i] = 0
	}
	rf.mu.Unlock()

	go rf.startHeartBeat()

	// leader work
	for {
		if rf.killed() {
			return
		}

		rf.mu.Lock()
		if rf.state == FOLLOWER {
			rf.mu.Unlock()
			go rf.follower()
			return
		}

		// log replication
		for server, index := range rf.nextIndex {
			if len(rf.log)-1 >= index {
				args := AppendEntriesArgs{
					rf.currentTerm, rf.me, index - 1,
					rf.log[index-1].Term, rf.log[index:],
					rf.commitIndex,
				}
				reply := AppendEntriesReply{}
				go rf.sendAppendEntries(server, &args, &reply)
			}
		}

		// commit log entry if possible
		for n := len(rf.log) - 1; n > rf.commitIndex; n-- {
			if rf.log[n].Term != rf.currentTerm {
				continue
			}
			// a majority of matchIndex[i] >= n
			matchNum := 0
			for _, index := range rf.matchIndex {
				if index >= n {
					matchNum++
				}
			}
			if matchNum > len(rf.peers)/2 {
				rf.commitIndex = n
				rf.applyCond.Broadcast()
				break
			}
		}

		rf.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (rf *Raft) candidate() {
	rf.mu.Lock()
	rf.currentTerm++
	rf.state = CANDIDATE
	rf.votedFor = rf.me
	rf.persist()
	rf.voteCount = 1 // vote for itself

	peerNum := len(rf.peers)
	term := rf.currentTerm
	me := rf.me
	lastLogIndex := len(rf.log) - 1
	lastLogTerm := rf.log[lastLogIndex].Term
	rf.mu.Unlock()

	rf.resetTimer()
	go rf.startElectionTimer()

	// send RequestVote to all peers
	for i := 0; i < peerNum; i++ {
		if i == me {
			continue
		}
		args := RequestVoteArgs{term, me, lastLogIndex, lastLogTerm}
		reply := RequestVoteReply{}
		go rf.sendRequestVote(i, &args, &reply)
	}

	// a candidate continues its state until one of three things happens
	// a conditional variable should be used here, but event a) b) and c)
	// are triggered by different goroutines, which increases complexity
	// therefore busy waiting is used here
	for {
		if rf.killed() {
			return
		}

		rf.mu.Lock()
		if rf.voteCount > peerNum/2 {
			// a) the candidate wins and becomes leader
			rf.mu.Unlock()
			go rf.leader()
			break
		}
		if rf.state == FOLLOWER {
			// b) another server establishes itself as leader
			rf.mu.Unlock()
			go rf.follower()
			break
		}
		if rf.currentTerm > term {
			// c) a certain peer has already started a new election
			// at this moment, this peer is either running follower() or candidate()
			rf.mu.Unlock()
			break
		}
		rf.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

// follower's behaviors are mostly handled by RPC handlers
func (rf *Raft) follower() {
	go rf.startElectionTimer()
}

// election timeout goroutine periodically checks
// whether the time since the last time heard from the leader is greater than the timeout period.
// If so, start a new election and return
// each time a server becomes a follower or starts an election, start this timer goroutine
func (rf *Raft) startElectionTimer() {
	for {
		if rf.killed() {
			return
		}
		rf.mu.Lock()
		electionTimeout := rf.electionTimeout
		lastHeard := rf.lastHeard
		rf.mu.Unlock()
		now := time.Now()
		if now.After(lastHeard.Add(electionTimeout)) {
			go rf.candidate()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

//
// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
// see paper's Figure 2 for a description of what should be persistent.
// this function can only be called when `rf` holds the lock
func (rf *Raft) persist() {
	// Your code here (2C).
	// Example:
	// w := new(bytes.Buffer)
	// e := labgob.NewEncoder(w)
	// e.Encode(rf.xxx)
	// e.Encode(rf.yyy)
	// data := w.Bytes()
	// rf.persister.SaveRaftState(data)
	buf := new(bytes.Buffer)
	enc := labgob.NewEncoder(buf)
	enc.Encode(rf.currentTerm)
	enc.Encode(rf.votedFor)
	enc.Encode(rf.log)
	data := buf.Bytes()
	rf.persister.SaveRaftState(data)
}

//
// restore previously persisted state.
//
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}
	// Your code here (2C).
	// Example:
	// r := bytes.NewBuffer(data)
	// d := labgob.NewDecoder(r)
	// var xxx
	// var yyy
	// if d.Decode(&xxx) != nil ||
	//    d.Decode(&yyy) != nil {
	//   error...
	// } else {
	//   rf.xxx = xxx
	//   rf.yyy = yyy
	// }
	buf := bytes.NewBuffer(data)
	de := labgob.NewDecoder(buf)
	var currentTerm int
	var votedFor int
	var log []Log
	count := 0
	for de.Decode(&currentTerm) != nil || de.Decode(&votedFor) != nil || de.Decode(&log) != nil {
		fmt.Fprintf(os.Stderr, "Peer #%d failed to decode state from persister, retrying...\n", rf.me)
		count++
		if count > 5 {
			panic("Peer #%d failed to decode state from persister, abort\n")
		}
	}
	rf.currentTerm = currentTerm
	rf.votedFor = votedFor
	rf.log = log
}

//
// A service wants to switch to snapshot.  Only do so if Raft hasn't
// have more recent info since it communicate the snapshot on applyCh.
//
func (rf *Raft) CondInstallSnapshot(lastIncludedTerm int, lastIncludedIndex int, snapshot []byte) bool {

	// Your code here (2D).

	return true
}

// the service says it has created a snapshot that has
// all info up to and including index. this means the
// service no longer needs the log through (and including)
// that index. Raft should now trim its log as much as possible.
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	// Your code here (2D).

}


//
// example RequestVote RPC arguments structure.
// field names must start with capital letters!
//
type RequestVoteArgs struct {
	// Your data here (2A, 2B).
	Term         int
	CandidateID  int
	LastLogIndex int
	LastLogTerm  int
}

//
// example RequestVote RPC reply structure.
// field names must start with capital letters!
//
type RequestVoteReply struct {
	// Your data here (2A).
	Term        int
	VoteGranted bool
}

//
// example RequestVote RPC handler.
//
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	// Your code here (2A, 2B).
	rf.mu.Lock()
	if args.Term < rf.currentTerm {
		reply.Term = rf.currentTerm
		reply.VoteGranted = false
		rf.mu.Unlock()
		return
	}
	// follow the second rule in "Rules for Servers" in figure 2 before handling an incoming RPC
	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.state = FOLLOWER
		rf.votedFor = -1
		rf.persist()
	}

	reply.Term = rf.currentTerm
	reply.VoteGranted = true
	// deny vote if already voted
	if rf.votedFor != -1 {
		reply.VoteGranted = false
		rf.mu.Unlock()
		return
	}
	// deny vote if consistency check fails (candidate is less up-to-date)
	lastLog := rf.log[len(rf.log)-1]
	if args.LastLogTerm < lastLog.Term || (args.LastLogTerm == lastLog.Term && args.LastLogIndex < len(rf.log)-1) {
		reply.VoteGranted = false
		rf.mu.Unlock()
		return
	}
	// now this peer must vote for the candidate
	rf.votedFor = args.CandidateID
	rf.mu.Unlock()

	rf.resetTimer()
}

type AppendEntriesArgs struct {
	Term         int
	LeaderID     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []Log
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term    int
	Success bool
	// for roll back optimization
	ConflictTerm  int
	ConflictIndex int
	LogLen        int
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rf.mu.Lock()
	if args.Term < rf.currentTerm {
		reply.Term = rf.currentTerm
		reply.Success = false
		rf.mu.Unlock()
		return
	}
	// follow the second rule in "Rules for Servers" in figure 2 before handling an incoming RPC
	if args.Term > rf.currentTerm {
		rf.currentTerm = args.Term
		rf.state = FOLLOWER
		rf.votedFor = -1
		rf.persist()
	}
	rf.mu.Unlock()
	// now we must have rf.currentTerm == args.Term, which means receiving from leader and reset timer
	rf.resetTimer()

	rf.mu.Lock()
	defer rf.mu.Unlock()

	// consistency check
	if len(rf.log)-1 < args.PrevLogIndex {
		// case 3: follower's log is too short
		reply.Term = rf.currentTerm
		reply.Success = false
		reply.ConflictIndex = 0
		reply.ConflictTerm = 0
		reply.LogLen = len(rf.log) - 1
		return
	}
	if rf.log[args.PrevLogIndex].Term != args.PrevLogTerm {
		// case 1, 2: conflict at entry PrevLogIndex
		reply.Term = rf.currentTerm
		reply.Success = false
		reply.ConflictTerm = rf.log[args.PrevLogIndex].Term
		for i := args.PrevLogIndex; i > 0; i-- {
			if rf.log[i-1].Term != reply.ConflictTerm {
				reply.ConflictIndex = i
				break
			}
		}
		return
	}
	// accept new entries
	reply.Term = rf.currentTerm
	reply.Success = true

	// log replication
	if len(args.Entries) == 0 {
		return
	}
	conflictEntry := -1
	for i := 0; i < len(args.Entries); i++ {
		if len(rf.log)-1 < args.PrevLogIndex+i+1 || args.Entries[i].Term != rf.log[args.PrevLogIndex+i+1].Term {
			// existing an entry conflicts with a new one, truncate the log
			rf.log = rf.log[:args.PrevLogIndex+i+1]
			conflictEntry = i
			break
		}
	}
	if conflictEntry != -1 {
		// need to append new entries to the log
		for i := conflictEntry; i < len(args.Entries); i++ {
			rf.log = append(rf.log, args.Entries[i])
		}
	}

	rf.persist() // log has changed

	// advance commitIndex if possible
	if args.LeaderCommit > rf.commitIndex {
		// BUG? index of last new entry == args.PrevLogIndex+len(args.Entries)) based on my comprehension
		rf.commitIndex = min(args.LeaderCommit, args.PrevLogIndex+len(args.Entries))
		rf.applyCond.Broadcast()
	}
}

// I just wonder why `math` package does not provide this simple function
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

//
// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
//
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) {
	if ok := rf.peers[server].Call("Raft.RequestVote", args, reply); !ok {
		return
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()

	// drop old reply
	if reply.Term != args.Term {
		return
	}

	if reply.Term > rf.currentTerm {
		rf.currentTerm = reply.Term
		rf.persist()
		rf.state = FOLLOWER
		return
	}
	if reply.VoteGranted {
		rf.voteCount++
	}
}

func (rf *Raft) sendAppendEntries(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) {
	if ok := rf.peers[server].Call("Raft.AppendEntries", args, reply); !ok {
		return
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()

	// drop old reply
	if args.Term != reply.Term {
		return
	}

	if reply.Term > rf.currentTerm {
		rf.currentTerm = reply.Term
		rf.persist()
		rf.state = FOLLOWER
		return
	}
	if reply.Success {
		rf.matchIndex[server] = args.PrevLogIndex + len(args.Entries)
		rf.nextIndex[server] = rf.matchIndex[server] + 1
		return
	} else {
		// roll back nextIndex for this follower
		if reply.ConflictIndex == 0 {
			// case 3: follower's log is too short
			rf.nextIndex[server] = reply.LogLen
		} else {
			hasTerm := false
			for i := len(rf.log) - 1; i > 0; i-- {
				if rf.log[i].Term == reply.ConflictTerm {
					// case 2: leader has conflictTerm
					hasTerm = true
					rf.nextIndex[server] = i + 1
					break
				}
			}
			if !hasTerm {
				// case 1: leader does not has conflictTerm
				rf.nextIndex[server] = reply.ConflictIndex
			}
		}
	}
}

//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	// Your code here (2B).
	rf.mu.Lock()
	defer rf.mu.Unlock()

	if rf.killed() || rf.state != LEADER {
		return len(rf.log), rf.currentTerm, false
	}

	rf.log = append(rf.log, Log{rf.currentTerm, command})
	rf.persist()

	return len(rf.log) - 1, rf.currentTerm, true
}

//
// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
//
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	// Your initialization code here (2A, 2B, 2C).
	rf.mu = sync.Mutex{}
	rf.state = FOLLOWER

	rf.currentTerm = 0
	rf.votedFor = -1
	rf.log = []Log{}
	// log index counts from 1, index 0 is filled with a fake log with term 0
	rf.log = append(rf.log, Log{0, nil})

	rf.commitIndex = 0
	rf.lastApplied = 0

	rf.nextIndex = make([]int, len(peers))
	rf.matchIndex = make([]int, len(peers))

	rf.applyCond = sync.NewCond(&rf.mu)

	rf.resetTimer()

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	// start as a follower
	go rf.follower()

	// start apply check as a daemon
	go rf.applier(applyCh)

	return rf
}
