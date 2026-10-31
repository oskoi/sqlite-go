package sqlite

/*
#include "bridge.h"
*/
import "C"

// OpenFlags selects native connection flags. Zero uses read/write, create, URI and no mutex.
type OpenFlags uint32

// PrepareFlags selects native preparation hints.
type PrepareFlags uint32

// Limit identifies a native per-connection limit.
type Limit int32

// DBStatus identifies a native connection status counter.
type DBStatus int32

// StmtStatus identifies a native statement status counter.
type StmtStatus int32

const (
	OpenReadOnly      OpenFlags    = C.SQLITE_OPEN_READONLY
	OpenReadWrite     OpenFlags    = C.SQLITE_OPEN_READWRITE
	OpenCreate        OpenFlags    = C.SQLITE_OPEN_CREATE
	OpenURI           OpenFlags    = C.SQLITE_OPEN_URI
	OpenMemory        OpenFlags    = C.SQLITE_OPEN_MEMORY
	OpenNoMutex       OpenFlags    = C.SQLITE_OPEN_NOMUTEX
	OpenFullMutex     OpenFlags    = C.SQLITE_OPEN_FULLMUTEX
	OpenPrivateCache  OpenFlags    = C.SQLITE_OPEN_PRIVATECACHE
	OpenNoFollow      OpenFlags    = C.SQLITE_OPEN_NOFOLLOW
	PreparePersistent PrepareFlags = C.SQLITE_PREPARE_PERSISTENT
	PrepareNoVTab     PrepareFlags = C.SQLITE_PREPARE_NO_VTAB
)
const (
	LimitLength            Limit = C.SQLITE_LIMIT_LENGTH
	LimitSQLLength         Limit = C.SQLITE_LIMIT_SQL_LENGTH
	LimitColumn            Limit = C.SQLITE_LIMIT_COLUMN
	LimitExprDepth         Limit = C.SQLITE_LIMIT_EXPR_DEPTH
	LimitCompoundSelect    Limit = C.SQLITE_LIMIT_COMPOUND_SELECT
	LimitVDBEOp            Limit = C.SQLITE_LIMIT_VDBE_OP
	LimitFunctionArg       Limit = C.SQLITE_LIMIT_FUNCTION_ARG
	LimitAttached          Limit = C.SQLITE_LIMIT_ATTACHED
	LimitLikePatternLength Limit = C.SQLITE_LIMIT_LIKE_PATTERN_LENGTH
	LimitVariableNumber    Limit = C.SQLITE_LIMIT_VARIABLE_NUMBER
	LimitTriggerDepth      Limit = C.SQLITE_LIMIT_TRIGGER_DEPTH
	LimitWorkerThreads     Limit = C.SQLITE_LIMIT_WORKER_THREADS
	LimitParserDepth       Limit = C.SQLITE_LIMIT_PARSER_DEPTH
)
const (
	DBStatusLookasideUsed     DBStatus = C.SQLITE_DBSTATUS_LOOKASIDE_USED
	DBStatusCacheUsed         DBStatus = C.SQLITE_DBSTATUS_CACHE_USED
	DBStatusSchemaUsed        DBStatus = C.SQLITE_DBSTATUS_SCHEMA_USED
	DBStatusStmtUsed          DBStatus = C.SQLITE_DBSTATUS_STMT_USED
	DBStatusLookasideHit      DBStatus = C.SQLITE_DBSTATUS_LOOKASIDE_HIT
	DBStatusLookasideMissSize DBStatus = C.SQLITE_DBSTATUS_LOOKASIDE_MISS_SIZE
	DBStatusLookasideMissFull DBStatus = C.SQLITE_DBSTATUS_LOOKASIDE_MISS_FULL
	DBStatusCacheHit          DBStatus = C.SQLITE_DBSTATUS_CACHE_HIT
	DBStatusCacheMiss         DBStatus = C.SQLITE_DBSTATUS_CACHE_MISS
	DBStatusCacheWrite        DBStatus = C.SQLITE_DBSTATUS_CACHE_WRITE
	DBStatusDeferredFKs       DBStatus = C.SQLITE_DBSTATUS_DEFERRED_FKS
	DBStatusCacheUsedShared   DBStatus = C.SQLITE_DBSTATUS_CACHE_USED_SHARED
	DBStatusCacheSpill        DBStatus = C.SQLITE_DBSTATUS_CACHE_SPILL
	DBStatusTempBufSpill      DBStatus = C.SQLITE_DBSTATUS_TEMPBUF_SPILL
)
const (
	StmtStatusFullScanStep StmtStatus = C.SQLITE_STMTSTATUS_FULLSCAN_STEP
	StmtStatusSort         StmtStatus = C.SQLITE_STMTSTATUS_SORT
	StmtStatusAutoIndex    StmtStatus = C.SQLITE_STMTSTATUS_AUTOINDEX
	StmtStatusVMStep       StmtStatus = C.SQLITE_STMTSTATUS_VM_STEP
	StmtStatusReprepare    StmtStatus = C.SQLITE_STMTSTATUS_REPREPARE
	StmtStatusRun          StmtStatus = C.SQLITE_STMTSTATUS_RUN
	StmtStatusFilterMiss   StmtStatus = C.SQLITE_STMTSTATUS_FILTER_MISS
	StmtStatusFilterHit    StmtStatus = C.SQLITE_STMTSTATUS_FILTER_HIT
	StmtStatusMemUsed      StmtStatus = C.SQLITE_STMTSTATUS_MEMUSED
)

// Native result codes, including extended results. Match primary families with errors.Is.
const (
	CodeOK                     Code = C.SQLITE_OK
	CodeError                  Code = C.SQLITE_ERROR
	CodeInternal               Code = C.SQLITE_INTERNAL
	CodePerm                   Code = C.SQLITE_PERM
	CodeAbort                  Code = C.SQLITE_ABORT
	CodeBusy                   Code = C.SQLITE_BUSY
	CodeLocked                 Code = C.SQLITE_LOCKED
	CodeNoMem                  Code = C.SQLITE_NOMEM
	CodeReadOnly               Code = C.SQLITE_READONLY
	CodeInterrupt              Code = C.SQLITE_INTERRUPT
	CodeIOErr                  Code = C.SQLITE_IOERR
	CodeCorrupt                Code = C.SQLITE_CORRUPT
	CodeNotFound               Code = C.SQLITE_NOTFOUND
	CodeFull                   Code = C.SQLITE_FULL
	CodeCantOpen               Code = C.SQLITE_CANTOPEN
	CodeProtocol               Code = C.SQLITE_PROTOCOL
	CodeEmpty                  Code = C.SQLITE_EMPTY
	CodeSchema                 Code = C.SQLITE_SCHEMA
	CodeTooBig                 Code = C.SQLITE_TOOBIG
	CodeConstraint             Code = C.SQLITE_CONSTRAINT
	CodeMismatch               Code = C.SQLITE_MISMATCH
	CodeMisuse                 Code = C.SQLITE_MISUSE
	CodeNoLFS                  Code = C.SQLITE_NOLFS
	CodeAuth                   Code = C.SQLITE_AUTH
	CodeFormat                 Code = C.SQLITE_FORMAT
	CodeRange                  Code = C.SQLITE_RANGE
	CodeNotADB                 Code = C.SQLITE_NOTADB
	CodeNotice                 Code = C.SQLITE_NOTICE
	CodeWarning                Code = C.SQLITE_WARNING
	CodeRow                    Code = C.SQLITE_ROW
	CodeDone                   Code = C.SQLITE_DONE
	CodeErrorMissingCollSeq    Code = C.SQLITE_ERROR_MISSING_COLLSEQ
	CodeErrorRetry             Code = C.SQLITE_ERROR_RETRY
	CodeErrorSnapshot          Code = C.SQLITE_ERROR_SNAPSHOT
	CodeErrorReserveSize       Code = C.SQLITE_ERROR_RESERVESIZE
	CodeErrorKey               Code = C.SQLITE_ERROR_KEY
	CodeErrorUnable            Code = C.SQLITE_ERROR_UNABLE
	CodeIOErrRead              Code = C.SQLITE_IOERR_READ
	CodeIOErrShortRead         Code = C.SQLITE_IOERR_SHORT_READ
	CodeIOErrWrite             Code = C.SQLITE_IOERR_WRITE
	CodeIOErrFsync             Code = C.SQLITE_IOERR_FSYNC
	CodeIOErrDirFsync          Code = C.SQLITE_IOERR_DIR_FSYNC
	CodeIOErrTruncate          Code = C.SQLITE_IOERR_TRUNCATE
	CodeIOErrFstat             Code = C.SQLITE_IOERR_FSTAT
	CodeIOErrUnlock            Code = C.SQLITE_IOERR_UNLOCK
	CodeIOErrRDLock            Code = C.SQLITE_IOERR_RDLOCK
	CodeIOErrDelete            Code = C.SQLITE_IOERR_DELETE
	CodeIOErrBlocked           Code = C.SQLITE_IOERR_BLOCKED
	CodeIOErrNoMem             Code = C.SQLITE_IOERR_NOMEM
	CodeIOErrAccess            Code = C.SQLITE_IOERR_ACCESS
	CodeIOErrCheckReservedLock Code = C.SQLITE_IOERR_CHECKRESERVEDLOCK
	CodeIOErrLock              Code = C.SQLITE_IOERR_LOCK
	CodeIOErrClose             Code = C.SQLITE_IOERR_CLOSE
	CodeIOErrDirClose          Code = C.SQLITE_IOERR_DIR_CLOSE
	CodeIOErrSHMOpen           Code = C.SQLITE_IOERR_SHMOPEN
	CodeIOErrSHMSize           Code = C.SQLITE_IOERR_SHMSIZE
	CodeIOErrSHMLock           Code = C.SQLITE_IOERR_SHMLOCK
	CodeIOErrSHMMap            Code = C.SQLITE_IOERR_SHMMAP
	CodeIOErrSeek              Code = C.SQLITE_IOERR_SEEK
	CodeIOErrDeleteNoEnt       Code = C.SQLITE_IOERR_DELETE_NOENT
	CodeIOErrMmap              Code = C.SQLITE_IOERR_MMAP
	CodeIOErrGetTempPath       Code = C.SQLITE_IOERR_GETTEMPPATH
	CodeIOErrConvPath          Code = C.SQLITE_IOERR_CONVPATH
	CodeIOErrVNode             Code = C.SQLITE_IOERR_VNODE
	CodeIOErrAuth              Code = C.SQLITE_IOERR_AUTH
	CodeIOErrBeginAtomic       Code = C.SQLITE_IOERR_BEGIN_ATOMIC
	CodeIOErrCommitAtomic      Code = C.SQLITE_IOERR_COMMIT_ATOMIC
	CodeIOErrRollbackAtomic    Code = C.SQLITE_IOERR_ROLLBACK_ATOMIC
	CodeIOErrData              Code = C.SQLITE_IOERR_DATA
	CodeIOErrCorruptFS         Code = C.SQLITE_IOERR_CORRUPTFS
	CodeIOErrInPage            Code = C.SQLITE_IOERR_IN_PAGE
	CodeIOErrBadKey            Code = C.SQLITE_IOERR_BADKEY
	CodeIOErrCodec             Code = C.SQLITE_IOERR_CODEC
	CodeLockedSharedCache      Code = C.SQLITE_LOCKED_SHAREDCACHE
	CodeLockedVTab             Code = C.SQLITE_LOCKED_VTAB
	CodeBusyRecovery           Code = C.SQLITE_BUSY_RECOVERY
	CodeBusySnapshot           Code = C.SQLITE_BUSY_SNAPSHOT
	CodeBusyTimeout            Code = C.SQLITE_BUSY_TIMEOUT
	CodeCantOpenNoTempDir      Code = C.SQLITE_CANTOPEN_NOTEMPDIR
	CodeCantOpenIsDir          Code = C.SQLITE_CANTOPEN_ISDIR
	CodeCantOpenFullPath       Code = C.SQLITE_CANTOPEN_FULLPATH
	CodeCantOpenConvPath       Code = C.SQLITE_CANTOPEN_CONVPATH
	CodeCantOpenDirtyWAL       Code = C.SQLITE_CANTOPEN_DIRTYWAL
	CodeCantOpenSymlink        Code = C.SQLITE_CANTOPEN_SYMLINK
	CodeCorruptVTab            Code = C.SQLITE_CORRUPT_VTAB
	CodeCorruptSequence        Code = C.SQLITE_CORRUPT_SEQUENCE
	CodeCorruptIndex           Code = C.SQLITE_CORRUPT_INDEX
	CodeReadOnlyRecovery       Code = C.SQLITE_READONLY_RECOVERY
	CodeReadOnlyCantLock       Code = C.SQLITE_READONLY_CANTLOCK
	CodeReadOnlyRollback       Code = C.SQLITE_READONLY_ROLLBACK
	CodeReadOnlyDBMoved        Code = C.SQLITE_READONLY_DBMOVED
	CodeReadOnlyCantInit       Code = C.SQLITE_READONLY_CANTINIT
	CodeReadOnlyDirectory      Code = C.SQLITE_READONLY_DIRECTORY
	CodeAbortRollback          Code = C.SQLITE_ABORT_ROLLBACK
	CodeConstraintCheck        Code = C.SQLITE_CONSTRAINT_CHECK
	CodeConstraintCommitHook   Code = C.SQLITE_CONSTRAINT_COMMITHOOK
	CodeConstraintForeignKey   Code = C.SQLITE_CONSTRAINT_FOREIGNKEY
	CodeConstraintFunction     Code = C.SQLITE_CONSTRAINT_FUNCTION
	CodeConstraintNotNull      Code = C.SQLITE_CONSTRAINT_NOTNULL
	CodeConstraintPrimaryKey   Code = C.SQLITE_CONSTRAINT_PRIMARYKEY
	CodeConstraintTrigger      Code = C.SQLITE_CONSTRAINT_TRIGGER
	CodeConstraintUnique       Code = C.SQLITE_CONSTRAINT_UNIQUE
	CodeConstraintVTab         Code = C.SQLITE_CONSTRAINT_VTAB
	CodeConstraintRowID        Code = C.SQLITE_CONSTRAINT_ROWID
	CodeConstraintPinned       Code = C.SQLITE_CONSTRAINT_PINNED
	CodeConstraintDataType     Code = C.SQLITE_CONSTRAINT_DATATYPE
	CodeNoticeRecoverWAL       Code = C.SQLITE_NOTICE_RECOVER_WAL
	CodeNoticeRecoverRollback  Code = C.SQLITE_NOTICE_RECOVER_ROLLBACK
	CodeNoticeRBU              Code = C.SQLITE_NOTICE_RBU
	CodeWarningAutoIndex       Code = C.SQLITE_WARNING_AUTOINDEX
	CodeAuthUser               Code = C.SQLITE_AUTH_USER
	CodeOKLoadPermanently      Code = C.SQLITE_OK_LOAD_PERMANENTLY
	CodeOKSymlink              Code = C.SQLITE_OK_SYMLINK
)

// Native row-change operations.
const (
	OperationInsert Operation = C.SQLITE_INSERT
	OperationUpdate Operation = C.SQLITE_UPDATE
	OperationDelete Operation = C.SQLITE_DELETE
)

// Native authorization outcomes.
const (
	AuthorizerOK     AuthorizerResult = C.SQLITE_OK
	AuthorizerDeny   AuthorizerResult = C.SQLITE_DENY
	AuthorizerIgnore AuthorizerResult = C.SQLITE_IGNORE
)

// Native authorization actions. Argument meanings follow sqlite3_set_authorizer.
const (
	AuthorizerCreateIndex       AuthorizerAction = C.SQLITE_CREATE_INDEX
	AuthorizerCreateTable       AuthorizerAction = C.SQLITE_CREATE_TABLE
	AuthorizerCreateTempIndex   AuthorizerAction = C.SQLITE_CREATE_TEMP_INDEX
	AuthorizerCreateTempTable   AuthorizerAction = C.SQLITE_CREATE_TEMP_TABLE
	AuthorizerCreateTempTrigger AuthorizerAction = C.SQLITE_CREATE_TEMP_TRIGGER
	AuthorizerCreateTempView    AuthorizerAction = C.SQLITE_CREATE_TEMP_VIEW
	AuthorizerCreateTrigger     AuthorizerAction = C.SQLITE_CREATE_TRIGGER
	AuthorizerCreateView        AuthorizerAction = C.SQLITE_CREATE_VIEW
	AuthorizerDelete            AuthorizerAction = C.SQLITE_DELETE
	AuthorizerDropIndex         AuthorizerAction = C.SQLITE_DROP_INDEX
	AuthorizerDropTable         AuthorizerAction = C.SQLITE_DROP_TABLE
	AuthorizerDropTempIndex     AuthorizerAction = C.SQLITE_DROP_TEMP_INDEX
	AuthorizerDropTempTable     AuthorizerAction = C.SQLITE_DROP_TEMP_TABLE
	AuthorizerDropTempTrigger   AuthorizerAction = C.SQLITE_DROP_TEMP_TRIGGER
	AuthorizerDropTempView      AuthorizerAction = C.SQLITE_DROP_TEMP_VIEW
	AuthorizerDropTrigger       AuthorizerAction = C.SQLITE_DROP_TRIGGER
	AuthorizerDropView          AuthorizerAction = C.SQLITE_DROP_VIEW
	AuthorizerInsert            AuthorizerAction = C.SQLITE_INSERT
	AuthorizerPragma            AuthorizerAction = C.SQLITE_PRAGMA
	AuthorizerRead              AuthorizerAction = C.SQLITE_READ
	AuthorizerSelect            AuthorizerAction = C.SQLITE_SELECT
	AuthorizerTransaction       AuthorizerAction = C.SQLITE_TRANSACTION
	AuthorizerUpdate            AuthorizerAction = C.SQLITE_UPDATE
	AuthorizerAttach            AuthorizerAction = C.SQLITE_ATTACH
	AuthorizerDetach            AuthorizerAction = C.SQLITE_DETACH
	AuthorizerAlterTable        AuthorizerAction = C.SQLITE_ALTER_TABLE
	AuthorizerReindex           AuthorizerAction = C.SQLITE_REINDEX
	AuthorizerAnalyze           AuthorizerAction = C.SQLITE_ANALYZE
	AuthorizerCreateVTable      AuthorizerAction = C.SQLITE_CREATE_VTABLE
	AuthorizerDropVTable        AuthorizerAction = C.SQLITE_DROP_VTABLE
	AuthorizerFunction          AuthorizerAction = C.SQLITE_FUNCTION
	AuthorizerSavepoint         AuthorizerAction = C.SQLITE_SAVEPOINT
	AuthorizerRecursive         AuthorizerAction = C.SQLITE_RECURSIVE
)
