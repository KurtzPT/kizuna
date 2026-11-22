------------------------------ MODULE AniDBCalls ------------------------------
(***************************************************************************)
(* The life of an AniDB API call, from the caller's fair use check to the  *)
(* outcome being recorded, across the two locks it crosses: the AniDB      *)
(* client's (c.mu in internal/sources/anidb_fairuse.go) and the fetch      *)
(* client's (c.mu and the host queue in internal/fetch).                   *)
(*                                                                         *)
(* Checked against the code at ecc89a6: anidb_fairuse.go, AniDB.ask in     *)
(* anidb.go, download, run and before in fetch.go, ticket.wait and sent in *)
(* throttle.go. A change there means walking the model against it again.   *)
(*                                                                         *)
(* Each action is one step the Go code takes under one lock, named after   *)
(* the function that takes it. Steps under different locks are separate   *)
(* actions, so TLC tries every order they can run in.                      *)
(*                                                                         *)
(* Time: the whole run fits in one 24 h window. A ban, a client pause, an  *)
(* anime's Failed or Fetched mark and the day's calls never expire here;   *)
(* only a backoff ends (BackoffEnds) and the 4 s gap passes (GapPasses).   *)
(*                                                                         *)
(* Run it with scripts/model.sh. The switches below bring back one past    *)
(* bug each (model/replay/*.cfg) or drop one of today's defences           *)
(* (model/defence/*.cfg). model/findings/*.cfg are bugs the model found    *)
(* that are not fixed yet.                                                 *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS
    DailyLimit,     \* anidbDailyLimit (200), scaled down
    BgLimit,        \* anidbBackgroundLimit (150), scaled down
    Outcomes,       \* what a request that went out can end with (Response)
    PanicInBefore,  \* sending may panic once its work is done
    PanicInDone,    \* result may panic in its log line for a ban or a client error
    \* Switches: TRUE is today's code, FALSE brings the old bug back.
    LeaveBeforeDone,       \* adf965d: fetch's run leaves inflight before Done
    SendingChecksPause,    \* ee1ebd7: sendingLocked checks paused again
    JustEndedGuard,        \* e8e215c: sendingLocked refuses a call already sent
    ResultGivesBackJoined, \* d40bf6a: result gives back a stopped call's joiner
    ClientErrorPauses,     \* bfc8b14: an error about the client pauses every call
    NoAnswerBlocksNoAnime, \* 9df93e2: a failure with no answer blocks no anime
    TakeBackFetched,       \* 0ac517c: result takes the Fetched mark back
    OnlyAnswerEndsBackoff, \* defence: bad XML keeps the backoff
    SendingCleansUp,       \* defence: sending's deferred cleanup on a panic
    SentDeferred,          \* defence: before's deferred t.sent()
    PauseBeforeLog,        \* e7bcf2b: result sets a ban or a client pause before it logs it
    AnyErrorGivesBack      \* e7bcf2b: result gives back a joiner's call whatever the error

\* The scenario: three callers of AniDB.anime. Caller 1 is a background
\* prefetch (prefetchWorker) and caller 2 a show build Plex waits for, both
\* for anime 1; caller 3 is a show build for anime 2.
Aids == {1, 2}
Callers == {1, 2, 3}
CallerAid == [c \in Callers |-> IF c = 3 THEN 2 ELSE 1]
Background == [c \in Callers |-> c = 1]

\* What AniDB answered about the anime: its entry, an <error> about it, XML
\* that doesn't parse, a ban, an error about the client. Each stays counted,
\* and the anime is not asked again for 24 h (admit's Failed and Fetched
\* checks, or the pause). The other outcomes, "netErr" and "panic", are no
\* answer about the anime.
AnswerKinds == {"ok", "entryErr", "badXML", "ban", "client"}

\* Download ids: a caller starts at most one. A constant set, so that TLC
\* names each step after its action; each action checks d is in dl.
Downloads == 1..Cardinality(Callers)

VARIABLES
    pc,          \* caller -> where it is in ask (anidb.go)
    mydl,        \* caller -> the download it created or joined (0: none)
    dl,          \* download id -> its fetch.call and run's progress
    calls,       \* call id -> the anidbCall (sequence: ids are indexes)
    calling,     \* aid -> c.calling[aid] (0: none)
    inflight,    \* aid -> fetch's c.inflight[key] (0: none)
    fetched,     \* aid -> c.state.Fetched has it
    failed,      \* aid -> c.state.Failed has it
    banned,      \* now.Before(c.state.BannedUntil)
    rejected,    \* c.state.ClientRejected is this client, within its 24 h
    backoff,     \* now.Before(c.state.RetryAt)
    stopped,     \* c.stopped (Stop ran)
    going,       \* hostLimit.going: a request let through hasn't gone out
    gapOK,       \* 4 s have passed since hostLimit.last
    \* Ghosts: what happened on the wire, which the code can't see.
    answeredFor, \* aid -> AniDB answered a request about it (AnswerKinds)
    okFor,       \* aid -> AniDB answered it with its entry, and it was kept
    outages,     \* how many outages a failure with no answer started
    backoffOwed, \* the latest outage is not over yet
    broken       \* the rules a step broke: "paused", "again", "share",
                 \* "stopping"

vars == <<pc, mydl, dl, calls, calling, inflight, fetched, failed, banned,
          rejected, backoff, stopped, going, gapOK, answeredFor, okFor,
          outages, backoffOwed, broken>>

\* An anidbCall. counted: its entry is in c.state.Sent. sent: call.sent.
\* stale: call.at.Before(c.backedOffAt). Ghosts: out, the request really
\* went out; outage, the value of outages when it did; outcome, what came
\* back ("none" while nothing did).
NewCall(a, bg) == [aid |-> a, counted |-> TRUE, sent |-> FALSE, fg |-> ~bg,
                   answered |-> FALSE, stale |-> FALSE, out |-> FALSE,
                   outage |-> 0, outcome |-> "none"]

CountOf(cs) == Cardinality({i \in DOMAIN cs : cs[i].counted})

\* paused (anidb_fairuse.go): a ban, a client rejection or a backoff.
Paused == banned \/ rejected \/ backoff

\* admit: why a new call for a may not be counted ("" = it may).
AdmitWhy(a, bg, cs) ==
    IF bg /\ stopped THEN "stopping"
    ELSE IF failed[a] THEN "failed"
    ELSE IF fetched[a] THEN "fetched"
    ELSE IF bg /\ CountOf(cs) >= BgLimit THEN "background share"
    ELSE IF CountOf(cs) >= DailyLimit THEN "daily limit"
    ELSE ""

Uncount(cs, i) == [cs EXCEPT ![i].counted = FALSE]

\* What is wrong with the call a step counted (old to new calls), for a
\* background caller (bg) or not: nothing, if it counted none.
BadAdmitReasons(bg, old, new) ==
    IF bg /\ Len(new) > Len(old) /\ CountOf(old) >= BgLimit
    THEN {"share"} ELSE {}

-----------------------------------------------------------------------------
(* Callers: AniDB.ask, one call each.                                       *)

\* allowed, under c.mu: paused, then join a call under way, then admit.
Allowed(c) ==
    LET a == CallerAid[c]
        bg == Background[c]
        cl == calling[a]
    IN /\ pc[c] = "start"
       /\ IF Paused THEN
             /\ pc' = [pc EXCEPT ![c] = "done"]
             /\ UNCHANGED <<calls, calling>>
          ELSE IF cl # 0 THEN \* "this caller just waits for it"
             /\ calls' = [calls EXCEPT ![cl].fg = @ \/ ~bg]
             /\ pc' = [pc EXCEPT ![c] = "get"]
             /\ UNCHANGED calling
          ELSE IF AdmitWhy(a, bg, calls) # "" THEN
             /\ pc' = [pc EXCEPT ![c] = "done"]
             /\ UNCHANGED <<calls, calling>>
          ELSE
             /\ calls' = Append(calls, NewCall(a, bg))
             /\ calling' = [calling EXCEPT ![a] = Len(calls) + 1]
             /\ pc' = [pc EXCEPT ![c] = "get"]
       /\ broken' = broken \cup BadAdmitReasons(bg, calls, calls')
       /\ UNCHANGED <<mydl, dl, inflight, fetched, failed, banned, rejected,
                      backoff, stopped, going, gapOK, answeredFor, okFor,
                      outages, backoffOwed>>

\* fetch.Client.download, under fetch's c.mu: join the download running for
\* the key, or start one (enqueue a ticket, go c.run). AniDB's Request has
\* no TTL, so Get reads no cache first (freshKnown never hits), and download
\* never sends it back to look in the cache again (errLookAgain needs a TTL,
\* since 0691253): a caller takes this step once.
Download(c) ==
    LET a == CallerAid[c]
        n == Len(dl) + 1
    IN /\ pc[c] = "get"
       /\ IF inflight[a] # 0 THEN
             /\ mydl' = [mydl EXCEPT ![c] = inflight[a]]
             /\ UNCHANGED <<dl, inflight>>
          ELSE
             /\ dl' = Append(dl, [aid |-> a, bg |-> Background[c],
                                  phase |-> "queued", call |-> 0,
                                  err |-> "none", left |-> FALSE,
                                  recorded |-> FALSE])
             /\ inflight' = [inflight EXCEPT ![a] = n]
             /\ mydl' = [mydl EXCEPT ![c] = n]
       /\ pc' = [pc EXCEPT ![c] = "wait"]
       /\ UNCHANGED <<calls, calling, fetched, failed, banned, rejected,
                      backoff, stopped, going, gapOK, answeredFor, okFor,
                      outages, backoffOwed, broken>>

\* download's select on cl.done: the caller has the outcome. (A caller
\* that gives up on ctx first changes nothing the model checks.)
Returns(c) ==
    /\ pc[c] = "wait"
    /\ dl[mydl[c]].phase = "closed"
    /\ pc' = [pc EXCEPT ![c] = "done"]
    /\ UNCHANGED <<mydl, dl, calls, calling, inflight, fetched, failed,
                   banned, rejected, backoff, stopped, going, gapOK,
                   answeredFor, okFor, outages, backoffOwed, broken>>

-----------------------------------------------------------------------------
(* Downloads: fetch.Client.run on its own goroutine.                        *)

\* ticket.wait (throttle.go): its turn, the last request out, 4 s passed.
\* Any waiting ticket may go: foreground-first order only narrows this.
Wait(d) ==
    /\ d \in DOMAIN dl
    /\ dl[d].phase = "queued"
    /\ ~going
    /\ gapOK
    /\ going' = TRUE
    /\ dl' = [dl EXCEPT ![d].phase = "turn"]
    /\ UNCHANGED <<pc, mydl, calls, calling, inflight, fetched, failed,
                   banned, rejected, backoff, stopped, gapOK, answeredFor,
                   okFor, outages, backoffOwed, broken>>

\* sendingLocked, as a value: the new calls and calling, the call it
\* returns (0: nil) and its error ("none" or "notSent").
SendingLocked(a, bg) ==
    LET call == calling[a]
        cs == calls
        cg == calling
        stop(why) == [calls |-> cs, calling |-> cg, ret |-> 0, err |-> "notSent"]
        stopAndEnd == [calls |-> Uncount(cs, call),
                       calling |-> [cg EXCEPT ![a] = 0],
                       ret |-> 0, err |-> "notSent"]
    IN
    IF SendingChecksPause /\ Paused THEN
        \* a ban or a backoff the answer ahead brought
        IF call # 0 /\ ~cs[call].sent THEN stopAndEnd ELSE stop("paused")
    ELSE IF JustEndedGuard /\ call # 0 /\ cs[call].sent THEN
        stop("the last call for this anime just ended")
    ELSE IF call # 0 /\ stopped /\ ~cs[call].fg THEN
        stopAndEnd \* "stopping"
    ELSE IF call = 0 \/ cs[call].sent THEN
        \* joined a call that ended: a new call, under admit's rules
        IF AdmitWhy(a, bg, cs) # "" THEN stop("admit")
        ELSE LET n == Len(cs) + 1
             IN [calls |-> [Append(cs, NewCall(a, bg)) EXCEPT ![n].sent = TRUE],
                 calling |-> [cg EXCEPT ![a] = n],
                 ret |-> n, err |-> "none"]
    ELSE [calls |-> [cs EXCEPT ![call].sent = TRUE], calling |-> cg,
          ret |-> call, err |-> "none"]

\* What is wrong with a request for a going out now. The pause is written
\* out here, not read from Paused, which Allowed and SendingLocked use: a
\* mistake there must not hide it. It also holds once result recorded
\* AniDB's ban or client error, whatever flag that set.
BadSendReasons(a) ==
    (IF \/ banned \/ rejected \/ backoff
        \/ \E d \in DOMAIN dl : dl[d].recorded /\ dl[d].err \in {"ban", "client"}
     THEN {"paused"} ELSE {}) \cup
    (IF answeredFor[a] THEN {"again"} ELSE {}) \cup
    \* Exit could cut the request off before its outcome is recorded, so
    \* once Stop ran, one goes out only while Plex waits for it.
    (IF stopped /\ ~\E c \in Callers : /\ ~Background[c]
                                        /\ CallerAid[c] = a
                                        /\ pc[c] \in {"get", "wait"}
     THEN {"stopping"} ELSE {})

\* fetch's before: Before (AniDB.sending, under c.mu), then t.sent(). The
\* request goes out when sending returns a call (call.sent: "it left the
\* 4 s queue and went out"). Or sending panics once its work is done (a
\* state write's log line): its deferred cleanup ends the call it would have
\* sent, the panic becomes the download's error (fetchSafe), and nothing
\* goes out.
Before(d) ==
    LET a == dl[d].aid
        earlier == IF calling[a] # 0 /\ calls[calling[a]].sent
                   THEN calling[a] ELSE 0
        s == SendingLocked(a, dl[d].bg)
        cl == s.calling[a]
        clean == SendingCleansUp /\ cl # 0 /\ cl # earlier
    IN /\ d \in DOMAIN dl
       /\ dl[d].phase = "turn"
       /\ \/ /\ calls' = IF s.ret # 0
                         THEN [s.calls EXCEPT ![s.ret].out = TRUE,
                                              ![s.ret].outage = outages]
                         ELSE s.calls
             /\ calling' = s.calling
             /\ dl' = [dl EXCEPT ![d].phase =
                                     IF s.err = "none" THEN "out" ELSE "fetched",
                                 ![d].call = s.ret, ![d].err = s.err]
             /\ broken' = broken \cup BadAdmitReasons(dl[d].bg, calls, s.calls)
                               \cup (IF s.ret # 0 THEN BadSendReasons(a) ELSE {})
             /\ going' = FALSE
             /\ gapOK' = FALSE
          \/ /\ PanicInBefore
             /\ calls' = IF clean THEN Uncount(s.calls, cl) ELSE s.calls
             /\ calling' = IF clean THEN [s.calling EXCEPT ![a] = 0]
                           ELSE s.calling
             /\ dl' = [dl EXCEPT ![d].phase = "fetched", ![d].call = 0,
                                 ![d].err = "panic"]
             /\ broken' = broken \cup BadAdmitReasons(dl[d].bg, calls, s.calls)
             \* defer t.sent(): without it the host's slot stays taken
             /\ going' = ~SentDeferred
             /\ gapOK' = IF SentDeferred THEN FALSE ELSE gapOK
       /\ UNCHANGED <<pc, mydl, inflight, fetched, failed, banned, rejected,
                      backoff, stopped, answeredFor, okFor, outages,
                      backoffOwed>>

\* The answer (or a network error) comes back, and Validate runs on it:
\* AniDB.answered (under c.mu) marks Fetched before the entry is cached.
\* "panic": the download panics after answered (0ac517c's case).
Response(d) ==
    LET a == dl[d].aid
        i == dl[d].call
    IN /\ d \in DOMAIN dl
       /\ dl[d].phase = "out"
       /\ \E o \in Outcomes :
             /\ calls' = [calls EXCEPT ![i].outcome = o,
                                       ![i].answered = (o \in {"ok", "panic"})]
             /\ fetched' = IF o \in {"ok", "panic"}
                           THEN [fetched EXCEPT ![a] = TRUE] ELSE fetched
             /\ answeredFor' = IF o \in AnswerKinds
                               THEN [answeredFor EXCEPT ![a] = TRUE]
                               ELSE answeredFor
             /\ okFor' = IF o = "ok" THEN [okFor EXCEPT ![a] = TRUE] ELSE okFor
             /\ dl' = [dl EXCEPT ![d].phase = "fetched", ![d].err = o]
       /\ UNCHANGED <<pc, mydl, calling, inflight, failed, banned, rejected,
                      backoff, stopped, going, gapOK, outages, backoffOwed,
                      broken>>

\* run: delete(c.inflight, id) under fetch's c.mu. Today before Done. The
\* c.ended.Add(1) under the same lock is read only by a request with a TTL
\* (see Download), so the model has no ended.
Leave(d) ==
    /\ d \in DOMAIN dl
    /\ dl[d].phase = "fetched"
    /\ ~dl[d].left
    /\ LeaveBeforeDone \/ dl[d].recorded
    /\ inflight' = [inflight EXCEPT ![dl[d].aid] = 0]
    /\ dl' = [dl EXCEPT ![d].left = TRUE]
    /\ UNCHANGED <<pc, mydl, calls, calling, fetched, failed, banned,
                   rejected, backoff, stopped, going, gapOK, answeredFor,
                   okFor, outages, backoffOwed, broken>>

\* run: r.Done(cl.err), that is AniDB.result(aid, call, err) under c.mu.
\* The ghosts outages and backoffOwed keep AniDB's rule apart from the
\* code's stale and backedOffAt: a failure with no answer starts a new
\* outage, unless its call went out before the latest outage started (its
\* outage stamp is older: a slow answer to the outage under way). Only the
\* backoff's end or AniDB's own answer ends an outage; XML that doesn't
\* parse may not be AniDB's.
\* PanicInDone: the slog.Error for a new ban or for a client error while
\* nothing is paused panics. run's recover logs it, and the rest of result
\* never runs. Today each comes after the pause it reports is set, as
\* every other log line there comes after the state writes the model has:
\* result's one save, whose failure is logged too, is deferred from its
\* top (since e7bcf2b), so a panic changes nothing here. Before e7bcf2b
\* (PauseBeforeLog) these two came before BannedUntil and ClientRejected
\* were set. The model has no panic in that save's own log line; a Go
\* test covers it.
Done(d) ==
    LET a == dl[d].aid
        i == dl[d].call
        err == dl[d].err
        cg == IF i # 0 /\ calling[a] = i THEN [calling EXCEPT ![a] = 0]
              ELSE calling
        joined == cg[a]
        \* a caller that came in after sending ended this download's call
        \* (stopped it, or panicked), and joined the download, left a new
        \* call counted that no download sends: it is given back
        giveBack == /\ ResultGivesBackJoined
                    /\ (err = "notSent" \/ AnyErrorGivesBack)
                    /\ joined # 0
                    /\ ~calls[joined].sent
        callingAfterGiveBack == IF giveBack THEN [cg EXCEPT ![a] = 0] ELSE cg
        callsAfterGiveBack == IF giveBack THEN Uncount(calls, joined) ELSE calls
        cs == IF i # 0 THEN Uncount(callsAfterGiveBack, i) ELSE callsAfterGiveBack
        \* "a slow answer, 4 s behind it: the same outage"
        sameOutage == i # 0 /\ calls[i].stale
        newOutage == /\ err \in {"netErr", "panic"}
                     /\ (i = 0 \/ calls[i].outage = outages)
    IN /\ d \in DOMAIN dl
       /\ dl[d].phase = "fetched"
       /\ ~dl[d].recorded
       /\ ~LeaveBeforeDone \/ dl[d].left
       /\ dl' = [dl EXCEPT ![d].recorded = TRUE]
       /\ outages' = IF newOutage THEN outages + 1 ELSE outages
       /\ backoffOwed' = IF newOutage THEN TRUE
                         ELSE IF err \in AnswerKinds \ {"badXML"} THEN FALSE
                         ELSE backoffOwed
       /\ calling' = callingAfterGiveBack
       /\ CASE err = "notSent" ->
               \* sending gave its count back
               /\ calls' = callsAfterGiveBack
               /\ UNCHANGED <<fetched, failed, banned, rejected, backoff>>
            [] err = "ok" ->
               /\ calls' = callsAfterGiveBack
               /\ failed' = [failed EXCEPT ![a] = FALSE]
               /\ backoff' = FALSE
               /\ UNCHANGED <<fetched, banned, rejected>>
            [] err = "ban" ->
               /\ calls' = callsAfterGiveBack
               /\ failed' = [failed EXCEPT ![a] = TRUE]
               /\ \/ banned' = TRUE
                  \/ PanicInDone /\ ~PauseBeforeLog /\ ~banned /\ UNCHANGED banned
               /\ UNCHANGED <<fetched, rejected, backoff>>
            [] err = "client" /\ ClientErrorPauses ->
               \* not this anime's fault: every call would get the same answer
               /\ calls' = callsAfterGiveBack
               /\ \/ rejected' = TRUE
                  \/ PanicInDone /\ ~PauseBeforeLog /\ ~Paused /\ UNCHANGED rejected
               /\ UNCHANGED <<fetched, failed, banned, backoff>>
            [] err \in {"entryErr", "badXML"} \/ (err = "client" /\ ~ClientErrorPauses) ->
               \* no usable entry ("client" only with ClientErrorPauses off).
               \* XML that doesn't parse may be a cut-off answer or some
               \* other server's page: the outage may go on.
               /\ calls' = callsAfterGiveBack
               /\ failed' = [failed EXCEPT ![a] = TRUE]
               /\ IF err = "badXML" /\ OnlyAnswerEndsBackoff
                  THEN UNCHANGED backoff
                  ELSE backoff' = FALSE
               /\ UNCHANGED <<fetched, banned, rejected>>
            [] OTHER -> \* "netErr", "panic": no answer about the anime
               /\ fetched' = IF TakeBackFetched /\ i # 0 /\ calls[i].answered
                             THEN [fetched EXCEPT ![a] = FALSE] ELSE fetched
               /\ failed' = IF NoAnswerBlocksNoAnime THEN failed
                            ELSE [failed EXCEPT ![a] = TRUE]
               /\ IF sameOutage
                  THEN /\ calls' = cs
                       /\ UNCHANGED backoff
                  ELSE \* c.backedOffAt = now: every call out so far is older
                       /\ calls' = [j \in DOMAIN cs |->
                                      IF cs[j].sent THEN [cs[j] EXCEPT !.stale = TRUE]
                                      ELSE cs[j]]
                       /\ backoff' = TRUE
               /\ UNCHANGED <<banned, rejected>>
       /\ UNCHANGED <<pc, mydl, inflight, stopped, going, gapOK, answeredFor,
                      okFor, broken>>

\* run: close(cl.done), deferred, so after Done: waiters wake up.
Close(d) ==
    /\ d \in DOMAIN dl
    /\ dl[d].phase = "fetched"
    /\ dl[d].left
    /\ dl[d].recorded
    /\ dl' = [dl EXCEPT ![d].phase = "closed"]
    /\ UNCHANGED <<pc, mydl, calls, calling, inflight, fetched, failed,
                   banned, rejected, backoff, stopped, going, gapOK,
                   answeredFor, okFor, outages, backoffOwed, broken>>

-----------------------------------------------------------------------------
(* The world.                                                               *)

\* AniDB.Stop: Kizuna starts shutting down.
Stop ==
    /\ ~stopped
    /\ stopped' = TRUE
    /\ UNCHANGED <<pc, mydl, dl, calls, calling, inflight, fetched, failed,
                   banned, rejected, backoff, going, gapOK, answeredFor,
                   okFor, outages, backoffOwed, broken>>

\* c.state.RetryAt passes: the outage's backoff is over.
BackoffEnds ==
    /\ backoff
    /\ backoff' = FALSE
    /\ backoffOwed' = FALSE
    /\ UNCHANGED <<pc, mydl, dl, calls, calling, inflight, fetched, failed,
                   banned, rejected, stopped, going, gapOK, answeredFor,
                   okFor, outages, broken>>

\* hostLimit.last + gap passes.
GapPasses ==
    /\ ~gapOK
    /\ gapOK' = TRUE
    /\ UNCHANGED <<pc, mydl, dl, calls, calling, inflight, fetched, failed,
                   banned, rejected, backoff, stopped, going, answeredFor,
                   okFor, outages, backoffOwed, broken>>

\* Every caller returned and every download ended.
Quiescent ==
    /\ \A c \in Callers : pc[c] = "done"
    /\ \A d \in DOMAIN dl : dl[d].phase = "closed"

\* The run is over: nothing more happens (not a deadlock).
Finished == Quiescent /\ UNCHANGED vars

Init ==
    /\ pc = [c \in Callers |-> "start"]
    /\ mydl = [c \in Callers |-> 0]
    /\ dl = <<>>
    /\ calls = <<>>
    /\ calling = [a \in Aids |-> 0]
    /\ inflight = [a \in Aids |-> 0]
    /\ fetched = [a \in Aids |-> FALSE]
    /\ failed = [a \in Aids |-> FALSE]
    /\ banned = FALSE
    /\ rejected = FALSE
    /\ backoff = FALSE
    /\ stopped = FALSE
    /\ going = FALSE
    /\ gapOK = TRUE
    /\ answeredFor = [a \in Aids |-> FALSE]
    /\ okFor = [a \in Aids |-> FALSE]
    /\ outages = 0
    /\ backoffOwed = FALSE
    /\ broken = {}

Next ==
    \/ \E c \in Callers : Allowed(c)
    \/ \E c \in Callers : Download(c)
    \/ \E c \in Callers : Returns(c)
    \/ \E d \in Downloads : Wait(d)
    \/ \E d \in Downloads : Before(d)
    \/ \E d \in Downloads : Response(d)
    \/ \E d \in Downloads : Leave(d)
    \/ \E d \in Downloads : Done(d)
    \/ \E d \in Downloads : Close(d)
    \/ Stop
    \/ BackoffEnds
    \/ GapPasses
    \/ Finished

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* The rules: AniDB's fair use terms, and what Kizuna promises on top.      *)

\* No request goes out while Kizuna's ban, client pause or backoff runs,
\* or once result recorded AniDB's ban or client error. BanKept and
\* BackoffKept tie the flags to what AniDB answered.
NoSendWhilePaused == "paused" \notin broken

\* Once Kizuna is stopping, no request goes out that Plex doesn't wait
\* for: exit could cut it off before its outcome is recorded.
NoBackgroundSendWhenStopping == "stopping" \notin broken

\* An anime AniDB answered about (AnswerKinds) is not asked again within
\* 24 h.
NotAskedAgain == "again" \notin broken

\* Once result recorded AniDB's ban, or its error about the client, every
\* call stays paused for the rest of the 24 h (the whole run here).
BanKept ==
    \A d \in DOMAIN dl :
        dl[d].recorded =>
            /\ dl[d].err = "ban" => banned
            /\ dl[d].err = "client" => rejected

\* After a failure with no answer from AniDB, the backoff holds until it
\* ends or AniDB answers again.
BackoffKept == backoffOwed => backoff

\* Every request out is counted in the day's calls, until it ends with no
\* answer from AniDB (those are given back on purpose).
SentIsCounted ==
    \A i \in DOMAIN calls :
        calls[i].out /\ calls[i].outcome \in AnswerKinds \cup {"none"}
            => calls[i].counted

\* Once every caller and download ended, every counted call is a request
\* that went out and that AniDB answered: no count is left over.
NothingCountedLeftOver ==
    Quiescent =>
        \A i \in DOMAIN calls :
            calls[i].counted => calls[i].out /\ calls[i].outcome \in AnswerKinds

\* The day's count, queued calls included, never passes the daily limit.
\* With SentIsCounted, so do the requests AniDB answered.
LimitKept == CountOf(calls) <= DailyLimit

\* A background call is counted only while fewer than BgLimit calls are:
\* Plex's own requests keep the rest of the daily limit.
BackgroundShareKept == "share" \notin broken

\* One request per anime on the wire at a time (fetch: one download per key).
OneRequestPerAnime ==
    \A a \in Aids : Cardinality({d \in DOMAIN dl :
                                    dl[d].aid = a /\ dl[d].phase = "out"}) <= 1

\* A call under way that is marked sent belongs to a download that has yet
\* to record its outcome. Kept past that, every later caller would join it
\* and be refused ("the last call for this anime just ended") until a
\* restart.
SentCallHasDownload ==
    \A a \in Aids :
        calling[a] # 0 /\ calls[calling[a]].sent =>
            \E d \in DOMAIN dl : dl[d].call = calling[a] /\ ~dl[d].recorded

\* The host's queue is held only by a request deciding whether to go out:
\* otherwise no AniDB request goes out again until a restart.
HostQueueMoves == going => \E d \in DOMAIN dl : dl[d].phase = "turn"

\* c.calling is empty once every caller and download ended.
CallingEmpty == Quiescent => \A a \in Aids : calling[a] = 0

\* A call that fails after AniDB's answer (a panic) blocks no anime: once
\* it is all over, Fetched holds only anime whose entry was kept.
FetchedOnlyIfKept == Quiescent => \A a \in Aids : fetched[a] => okFor[a]

\* A failure with no answer from AniDB blocks no anime: Failed holds only
\* anime AniDB answered about.
FailedOnlyIfAnswered == \A a \in Aids : failed[a] => answeredFor[a]

=============================================================================
