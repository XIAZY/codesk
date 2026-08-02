-------------------- MODULE ReversibleRootTombstone --------------------
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    ReplicaA,
    ReplicaB,
    OperationA,
    OperationB,
    RestoreOperationA,
    RestoreOperationB,
    NoOp,
    NoRestoreOp,
    NoReplica,
    WindowTicks,
    MaxTime,
    MaxGeneration,
    MaxCopies,
    MaxRestarts,
    UseGenerationCAS,
    EnableTime,
    EnableNamespaceChanges,
    EnableNonContent,
    EnableExternalTombstone,
    EnableCrash

Replicas == {ReplicaA, ReplicaB}
Operations == {OperationA, OperationB}
RestoreOperations == {RestoreOperationA, RestoreOperationB}
Origin(op) == IF op = OperationA THEN ReplicaA ELSE ReplicaB
RestoreOpFor(op) == IF op = OperationA THEN RestoreOperationA ELSE RestoreOperationB
TombstoneOpFor(restoreOp) ==
    IF restoreOp = RestoreOperationA THEN OperationA ELSE OperationB

ASSUME /\ ReplicaA # ReplicaB
       /\ OperationA # OperationB
       /\ RestoreOperationA # RestoreOperationB
       /\ RestoreOperations \cap Operations = {}
       /\ NoOp \notin Operations
       /\ NoOp \notin RestoreOperations
       /\ NoRestoreOp \notin RestoreOperations
       /\ NoRestoreOp \notin Operations
       /\ NoReplica \notin Replicas
       /\ WindowTicks > 0
       /\ MaxTime >= WindowTicks
       /\ MaxGeneration >= 2
       /\ MaxCopies >= 2
       /\ MaxRestarts >= 0
       /\ UseGenerationCAS \in BOOLEAN
       /\ EnableTime \in BOOLEAN
       /\ EnableNamespaceChanges \in BOOLEAN
       /\ EnableNonContent \in BOOLEAN
       /\ EnableExternalTombstone \in BOOLEAN
       /\ EnableCrash \in BOOLEAN

RootStates == {"active", "tombstoned"}
NamespaceStates == {"matching", "changed", "conflicting"}
Occupants == {"content", "absent", "non-content"}
Phases == {
    "idle",
    "tombstone_pending",
    "window_open",
    "content_syncing",
    "restore_pending",
    "projection_pending"
}

OpFor(r) == IF r = ReplicaA THEN OperationA ELSE OperationB

VARIABLES
    now,
    root,
    namespace,
    windowGeneration,
    windowOp,
    windowOrigin,
    windowDeadline,
    windowConsumed,
    windowRestoreOp,
    phase,
    occupant,
    workflowOp,
    expectedGeneration,
    preparedGeneration,
    tombstoneMessages,
    restoreMessages,
    backendFrontier,
    requiredFrontier,
    tombstoneAccepts,
    restoreCommits,
    commitProof,
    commitActor,
    commitAt,
    commitDeadline,
    restarts,
    unsafeProjection

vars == <<
    now,
    root,
    namespace,
    windowGeneration,
    windowOp,
    windowOrigin,
    windowDeadline,
    windowConsumed,
    windowRestoreOp,
    phase,
    occupant,
    workflowOp,
    expectedGeneration,
    preparedGeneration,
    tombstoneMessages,
    restoreMessages,
    backendFrontier,
    requiredFrontier,
    tombstoneAccepts,
    restoreCommits,
    commitProof,
    commitActor,
    commitAt,
    commitDeadline,
    restarts,
    unsafeProjection
>>

Init ==
    /\ now = 0
    /\ root = "active"
    /\ namespace = "matching"
    /\ windowGeneration = 0
    /\ windowOp = NoOp
    /\ windowOrigin = NoReplica
    /\ windowDeadline = 0
    /\ windowConsumed = FALSE
    /\ windowRestoreOp = NoRestoreOp
    /\ phase = [r \in Replicas |-> "idle"]
    /\ occupant = [r \in Replicas |-> "content"]
    /\ workflowOp = [r \in Replicas |-> NoOp]
    /\ expectedGeneration = [r \in Replicas |-> 0]
    /\ preparedGeneration = [op \in Operations |-> 0]
    /\ tombstoneMessages = [op \in Operations |-> 0]
    /\ restoreMessages = [restoreOp \in RestoreOperations |-> 0]
    /\ backendFrontier = 0
    /\ requiredFrontier = [r \in Replicas |-> 0]
    /\ tombstoneAccepts = [op \in Operations |-> 0]
    /\ restoreCommits = [op \in Operations |-> 0]
    /\ commitProof = [op \in Operations |-> FALSE]
    /\ commitActor = [op \in Operations |-> NoReplica]
    /\ commitAt = [op \in Operations |-> 0]
    /\ commitDeadline = [op \in Operations |-> 0]
    /\ restarts = [r \in Replicas |-> 0]
    /\ unsafeProjection = FALSE

ObserveAbsent(r) ==
    /\ occupant[r] # "absent"
    /\ occupant' = [occupant EXCEPT ![r] = "absent"]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, workflowOp, expectedGeneration,
                    preparedGeneration, tombstoneMessages, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

ObserveContent(r) ==
    /\ occupant[r] # "content"
    /\ occupant' = [occupant EXCEPT ![r] = "content"]
    /\ phase' = [phase EXCEPT ![r] =
            IF phase[r] \in {"window_open", "restore_pending"}
            THEN "content_syncing"
            ELSE phase[r]]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, workflowOp, expectedGeneration,
                    preparedGeneration, tombstoneMessages, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

ObserveNonContent(r) ==
    /\ EnableNonContent
    /\ occupant[r] # "non-content"
    /\ occupant' = [occupant EXCEPT ![r] = "non-content"]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, workflowOp, expectedGeneration,
                    preparedGeneration, tombstoneMessages, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

BeginTombstone(r) ==
    LET op == OpFor(r) IN
    /\ phase[r] = "idle"
    /\ occupant[r] = "absent"
    /\ namespace = "matching"
    /\ tombstoneAccepts[op] = 0
    /\ phase' = [phase EXCEPT ![r] = "tombstone_pending"]
    /\ workflowOp' = [workflowOp EXCEPT ![r] = op]
    /\ expectedGeneration' = [expectedGeneration EXCEPT ![r] = windowGeneration]
    /\ preparedGeneration' = [preparedGeneration EXCEPT ![op] = windowGeneration]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, occupant, tombstoneMessages,
                    restoreMessages, backendFrontier, requiredFrontier,
                    tombstoneAccepts, restoreCommits, commitProof,
                    commitActor, commitAt, commitDeadline, restarts,
                    unsafeProjection>>

SendTombstone(r) ==
    LET op == workflowOp[r] IN
    /\ phase[r] = "tombstone_pending"
    /\ op \in Operations
    /\ tombstoneMessages[op] < MaxCopies
    /\ tombstoneMessages' = [tombstoneMessages EXCEPT ![op] = @ + 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

DeliverCurrentTombstone(op) ==
    /\ op \in Operations
    /\ tombstoneMessages[op] > 0
    /\ windowOp = op
    /\ preparedGeneration[op] = windowGeneration - 1
    /\ tombstoneMessages' = [tombstoneMessages EXCEPT ![op] = @ - 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

NewTombstoneAdmissible(op) ==
    /\ op \in Operations
    /\ op # windowOp
    /\ namespace = "matching"
    /\ windowGeneration < MaxGeneration
    /\ (~UseGenerationCAS \/ preparedGeneration[op] = windowGeneration)

DeliverNewTombstone(op) ==
    /\ tombstoneMessages[op] > 0
    /\ NewTombstoneAdmissible(op)
    /\ tombstoneMessages' = [tombstoneMessages EXCEPT ![op] = @ - 1]
    /\ root' = "tombstoned"
    /\ windowGeneration' = windowGeneration + 1
    /\ windowOp' = op
    /\ windowOrigin' = Origin(op)
    /\ windowDeadline' = now + WindowTicks
    /\ windowConsumed' = FALSE
    /\ windowRestoreOp' = NoRestoreOp
    /\ tombstoneAccepts' = [tombstoneAccepts EXCEPT ![op] = @ + 1]
    /\ UNCHANGED <<now, namespace, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration, restoreMessages,
                    backendFrontier, requiredFrontier, restoreCommits,
                    commitProof, commitActor, commitAt, commitDeadline,
                    restarts, unsafeProjection>>

RejectTombstone(op) ==
    /\ op \in Operations
    /\ tombstoneMessages[op] > 0
    /\ op # windowOp
    /\ ~NewTombstoneAdmissible(op)
    /\ tombstoneMessages' = [tombstoneMessages EXCEPT ![op] = @ - 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

ObserveWindow(r) ==
    /\ phase[r] = "tombstone_pending"
    /\ windowOp = workflowOp[r]
    /\ windowOrigin = r
    /\ phase' = [phase EXCEPT ![r] =
            IF occupant[r] = "content" THEN "content_syncing" ELSE "window_open"]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, occupant, workflowOp, expectedGeneration,
                    preparedGeneration, tombstoneMessages, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

ObserveFrontier(r) ==
    /\ phase[r] = "content_syncing"
    /\ occupant[r] = "content"
    /\ phase' = [phase EXCEPT ![r] = "restore_pending"]
    /\ backendFrontier' = 1
    /\ requiredFrontier' = [requiredFrontier EXCEPT ![r] = 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, occupant, workflowOp, expectedGeneration,
                    preparedGeneration, tombstoneMessages, restoreMessages,
                    tombstoneAccepts, restoreCommits, commitProof, commitActor,
                    commitAt, commitDeadline, restarts, unsafeProjection>>

SendRestore(r) ==
    LET op == workflowOp[r]
        restoreOp == RestoreOpFor(op) IN
    /\ phase[r] = "restore_pending"
    /\ op \in Operations
    /\ restoreMessages[restoreOp] < MaxCopies
    /\ restoreMessages' = [restoreMessages EXCEPT ![restoreOp] = @ + 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration,
                    tombstoneMessages, backendFrontier, requiredFrontier,
                    tombstoneAccepts, restoreCommits, commitProof,
                    commitActor, commitAt, commitDeadline, restarts,
                    unsafeProjection>>

CanCommitRestore(op, restoreOp) ==
    /\ op \in Operations
    /\ restoreOp = RestoreOpFor(op)
    /\ windowOp = op
    /\ windowOrigin = Origin(op)
    /\ windowGeneration = preparedGeneration[op] + 1
    /\ ~windowConsumed
    /\ root = "tombstoned"
    /\ namespace = "matching"
    /\ now < windowDeadline
    /\ backendFrontier >= requiredFrontier[Origin(op)]

DeliverRestore(restoreOp) ==
    LET op == TombstoneOpFor(restoreOp) IN
    /\ restoreOp \in RestoreOperations
    /\ restoreMessages[restoreOp] > 0
    /\ CanCommitRestore(op, restoreOp)
    /\ restoreMessages' = [restoreMessages EXCEPT ![restoreOp] = @ - 1]
    /\ root' = "active"
    /\ windowConsumed' = TRUE
    /\ windowRestoreOp' = restoreOp
    /\ restoreCommits' = [restoreCommits EXCEPT ![op] = @ + 1]
    /\ commitProof' = [commitProof EXCEPT ![op] =
            /\ backendFrontier >= requiredFrontier[Origin(op)]
            /\ windowGeneration = preparedGeneration[op] + 1]
    /\ commitActor' = [commitActor EXCEPT ![op] = Origin(op)]
    /\ commitAt' = [commitAt EXCEPT ![op] = now]
    /\ commitDeadline' = [commitDeadline EXCEPT ![op] = windowDeadline]
    /\ UNCHANGED <<now, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration,
                    tombstoneMessages, backendFrontier, requiredFrontier,
                    tombstoneAccepts, restarts, unsafeProjection>>

ReplayRestore(restoreOp) ==
    LET op == TombstoneOpFor(restoreOp) IN
    /\ restoreOp \in RestoreOperations
    /\ restoreMessages[restoreOp] > 0
    /\ windowOp = op
    /\ windowGeneration = preparedGeneration[op] + 1
    /\ windowConsumed
    /\ windowRestoreOp = restoreOp
    /\ restoreMessages' = [restoreMessages EXCEPT ![restoreOp] = @ - 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration,
                    tombstoneMessages, backendFrontier, requiredFrontier,
                    tombstoneAccepts, restoreCommits, commitProof,
                    commitActor, commitAt, commitDeadline, restarts,
                    unsafeProjection>>

RejectRestore(restoreOp) ==
    LET op == TombstoneOpFor(restoreOp) IN
    /\ restoreOp \in RestoreOperations
    /\ restoreMessages[restoreOp] > 0
    /\ ~CanCommitRestore(op, restoreOp)
    /\ ~(windowOp = op /\ windowConsumed /\ windowRestoreOp = restoreOp)
    /\ restoreMessages' = [restoreMessages EXCEPT ![restoreOp] = @ - 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration,
                    tombstoneMessages, backendFrontier, requiredFrontier,
                    tombstoneAccepts, restoreCommits, commitProof,
                    commitActor, commitAt, commitDeadline, restarts,
                    unsafeProjection>>

ObserveRestore(r) ==
    /\ phase[r] = "restore_pending"
    /\ windowOp = workflowOp[r]
    /\ windowOrigin = r
    /\ windowConsumed
    /\ windowRestoreOp = RestoreOpFor(workflowOp[r])
    /\ phase' = [phase EXCEPT ![r] = "projection_pending"]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, occupant, workflowOp, expectedGeneration,
                    preparedGeneration, tombstoneMessages, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

FinishProjection(r) ==
    /\ phase[r] = "projection_pending"
    /\ root = "active"
    /\ namespace = "matching"
    /\ occupant[r] = "content"
    /\ windowOp = workflowOp[r]
    /\ windowOrigin = r
    /\ windowConsumed
    /\ phase' = [phase EXCEPT ![r] = "idle"]
    /\ workflowOp' = [workflowOp EXCEPT ![r] = NoOp]
    /\ expectedGeneration' = [expectedGeneration EXCEPT ![r] = 0]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, occupant, preparedGeneration,
                    tombstoneMessages, restoreMessages, backendFrontier,
                    requiredFrontier, tombstoneAccepts, restoreCommits,
                    commitProof, commitActor, commitAt, commitDeadline,
                    restarts, unsafeProjection>>

ObserveSuperseded(r) ==
    /\ phase[r] \in {"window_open", "content_syncing", "restore_pending", "projection_pending"}
    /\ windowOp # workflowOp[r]
    /\ phase' = [phase EXCEPT ![r] = "idle"]
    /\ workflowOp' = [workflowOp EXCEPT ![r] = NoOp]
    /\ expectedGeneration' = [expectedGeneration EXCEPT ![r] = 0]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, occupant, preparedGeneration,
                    tombstoneMessages, restoreMessages, backendFrontier,
                    requiredFrontier, tombstoneAccepts, restoreCommits,
                    commitProof, commitActor, commitAt, commitDeadline,
                    restarts, unsafeProjection>>

AdvanceTime ==
    /\ EnableTime
    /\ now < MaxTime
    /\ now' = now + 1
    /\ UNCHANGED <<root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration,
                    tombstoneMessages, restoreMessages, backendFrontier,
                    requiredFrontier, tombstoneAccepts, restoreCommits,
                    commitProof, commitActor, commitAt, commitDeadline,
                    restarts, unsafeProjection>>

ChangeNamespace(nextNamespace) ==
    /\ EnableNamespaceChanges
    /\ nextNamespace \in NamespaceStates \ {"matching"}
    /\ namespace # nextNamespace
    /\ namespace' = nextNamespace
    /\ UNCHANGED <<now, root, windowGeneration, windowOp, windowOrigin,
                    windowDeadline, windowConsumed, windowRestoreOp, phase,
                    occupant, workflowOp, expectedGeneration,
                    preparedGeneration, tombstoneMessages, restoreMessages,
                    backendFrontier, requiredFrontier, tombstoneAccepts,
                    restoreCommits, commitProof, commitActor, commitAt,
                    commitDeadline, restarts, unsafeProjection>>

ExternalTombstone ==
    /\ EnableExternalTombstone
    /\ root = "active"
    /\ root' = "tombstoned"
    /\ UNCHANGED <<now, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration,
                    tombstoneMessages, restoreMessages, backendFrontier,
                    requiredFrontier, tombstoneAccepts, restoreCommits,
                    commitProof, commitActor, commitAt, commitDeadline,
                    restarts, unsafeProjection>>

CrashRestart(r) ==
    /\ EnableCrash
    /\ restarts[r] < MaxRestarts
    /\ restarts' = [restarts EXCEPT ![r] = @ + 1]
    /\ UNCHANGED <<now, root, namespace, windowGeneration, windowOp,
                    windowOrigin, windowDeadline, windowConsumed,
                    windowRestoreOp, phase, occupant, workflowOp,
                    expectedGeneration, preparedGeneration,
                    tombstoneMessages, restoreMessages, backendFrontier,
                    requiredFrontier, tombstoneAccepts, restoreCommits,
                    commitProof, commitActor, commitAt, commitDeadline,
                    unsafeProjection>>

Next ==
    \/ \E r \in Replicas : ObserveAbsent(r)
    \/ \E r \in Replicas : ObserveContent(r)
    \/ \E r \in Replicas : ObserveNonContent(r)
    \/ \E r \in Replicas : BeginTombstone(r)
    \/ \E r \in Replicas : SendTombstone(r)
    \/ \E op \in Operations : DeliverCurrentTombstone(op)
    \/ \E op \in Operations : DeliverNewTombstone(op)
    \/ \E op \in Operations : RejectTombstone(op)
    \/ \E r \in Replicas : ObserveWindow(r)
    \/ \E r \in Replicas : ObserveFrontier(r)
    \/ \E r \in Replicas : SendRestore(r)
    \/ \E restoreOp \in RestoreOperations : DeliverRestore(restoreOp)
    \/ \E restoreOp \in RestoreOperations : ReplayRestore(restoreOp)
    \/ \E restoreOp \in RestoreOperations : RejectRestore(restoreOp)
    \/ \E r \in Replicas : ObserveRestore(r)
    \/ \E r \in Replicas : FinishProjection(r)
    \/ \E r \in Replicas : ObserveSuperseded(r)
    \/ AdvanceTime
    \/ \E nextNamespace \in NamespaceStates \ {"matching"} : ChangeNamespace(nextNamespace)
    \/ ExternalTombstone
    \/ \E r \in Replicas : CrashRestart(r)

TypeOK ==
    /\ now \in 0..MaxTime
    /\ root \in RootStates
    /\ namespace \in NamespaceStates
    /\ windowGeneration \in 0..MaxGeneration
    /\ windowOp \in Operations \cup {NoOp}
    /\ windowOrigin \in Replicas \cup {NoReplica}
    /\ windowDeadline \in Nat
    /\ windowConsumed \in BOOLEAN
    /\ windowRestoreOp \in RestoreOperations \cup {NoRestoreOp}
    /\ phase \in [Replicas -> Phases]
    /\ occupant \in [Replicas -> Occupants]
    /\ workflowOp \in [Replicas -> Operations \cup {NoOp}]
    /\ expectedGeneration \in [Replicas -> 0..MaxGeneration]
    /\ preparedGeneration \in [Operations -> 0..MaxGeneration]
    /\ tombstoneMessages \in [Operations -> 0..MaxCopies]
    /\ restoreMessages \in [RestoreOperations -> 0..MaxCopies]
    /\ backendFrontier \in 0..1
    /\ requiredFrontier \in [Replicas -> 0..1]
    /\ tombstoneAccepts \in [Operations -> 0..MaxGeneration]
    /\ restoreCommits \in [Operations -> 0..MaxGeneration]
    /\ commitProof \in [Operations -> BOOLEAN]
    /\ commitActor \in [Operations -> Replicas \cup {NoReplica}]
    /\ commitAt \in [Operations -> Nat]
    /\ commitDeadline \in [Operations -> Nat]
    /\ restarts \in [Replicas -> 0..MaxRestarts]
    /\ unsafeProjection \in BOOLEAN
    /\ (windowOp = NoOp) = (windowGeneration = 0)
    /\ (windowOp = NoOp) = (windowOrigin = NoReplica)
    /\ windowConsumed => windowRestoreOp = RestoreOpFor(windowOp)
    /\ ~windowConsumed => windowRestoreOp = NoRestoreOp
    /\ \A r \in Replicas :
            (phase[r] = "idle") = (workflowOp[r] = NoOp)

OperationAtMostOnce ==
    \A op \in Operations :
        /\ tombstoneAccepts[op] <= 1
        /\ restoreCommits[op] <= 1

RestoreSafety ==
    \A op \in Operations :
        restoreCommits[op] = 1 =>
            /\ tombstoneAccepts[op] = 1
            /\ commitProof[op]
            /\ commitActor[op] = Origin(op)
            /\ commitAt[op] < commitDeadline[op]

ProjectionSafety == ~unsafeProjection

Safety ==
    /\ OperationAtMostOnce
    /\ RestoreSafety
    /\ ProjectionSafety

Spec == Init /\ [][Next]_vars

=============================================================================
