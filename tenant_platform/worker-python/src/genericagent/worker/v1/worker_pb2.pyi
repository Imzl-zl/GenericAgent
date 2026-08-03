import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class TerminalStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TERMINAL_STATUS_UNSPECIFIED: _ClassVar[TerminalStatus]
    TASK_SUCCEEDED: _ClassVar[TerminalStatus]
    TASK_FAILED: _ClassVar[TerminalStatus]
    TASK_CANCELLED: _ClassVar[TerminalStatus]
    TASK_INTERRUPTED: _ClassVar[TerminalStatus]
TERMINAL_STATUS_UNSPECIFIED: TerminalStatus
TASK_SUCCEEDED: TerminalStatus
TASK_FAILED: TerminalStatus
TASK_CANCELLED: TerminalStatus
TASK_INTERRUPTED: TerminalStatus

class RuntimePolicy(_message.Message):
    __slots__ = ("max_turns", "max_history_bytes", "max_working_bytes", "max_output_bytes", "task_timeout_seconds", "capability_version", "policy_digest")
    MAX_TURNS_FIELD_NUMBER: _ClassVar[int]
    MAX_HISTORY_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_WORKING_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_OUTPUT_BYTES_FIELD_NUMBER: _ClassVar[int]
    TASK_TIMEOUT_SECONDS_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_VERSION_FIELD_NUMBER: _ClassVar[int]
    POLICY_DIGEST_FIELD_NUMBER: _ClassVar[int]
    max_turns: int
    max_history_bytes: int
    max_working_bytes: int
    max_output_bytes: int
    task_timeout_seconds: int
    capability_version: str
    policy_digest: str
    def __init__(self, max_turns: _Optional[int] = ..., max_history_bytes: _Optional[int] = ..., max_working_bytes: _Optional[int] = ..., max_output_bytes: _Optional[int] = ..., task_timeout_seconds: _Optional[int] = ..., capability_version: _Optional[str] = ..., policy_digest: _Optional[str] = ...) -> None: ...

class StartSessionRequest(_message.Message):
    __slots__ = ("session_key", "snapshot_ref", "runtime_policy", "snapshot_id", "snapshot_checksum", "workspace_key", "runner_generation", "max_bundle_bytes")
    SESSION_KEY_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_REF_FIELD_NUMBER: _ClassVar[int]
    RUNTIME_POLICY_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_ID_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_KEY_FIELD_NUMBER: _ClassVar[int]
    RUNNER_GENERATION_FIELD_NUMBER: _ClassVar[int]
    MAX_BUNDLE_BYTES_FIELD_NUMBER: _ClassVar[int]
    session_key: str
    snapshot_ref: str
    runtime_policy: RuntimePolicy
    snapshot_id: str
    snapshot_checksum: str
    workspace_key: str
    runner_generation: int
    max_bundle_bytes: int
    def __init__(self, session_key: _Optional[str] = ..., snapshot_ref: _Optional[str] = ..., runtime_policy: _Optional[_Union[RuntimePolicy, _Mapping]] = ..., snapshot_id: _Optional[str] = ..., snapshot_checksum: _Optional[str] = ..., workspace_key: _Optional[str] = ..., runner_generation: _Optional[int] = ..., max_bundle_bytes: _Optional[int] = ...) -> None: ...

class StartSessionResponse(_message.Message):
    __slots__ = ("session_key", "worker_instance_id")
    SESSION_KEY_FIELD_NUMBER: _ClassVar[int]
    WORKER_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    session_key: str
    worker_instance_id: str
    def __init__(self, session_key: _Optional[str] = ..., worker_instance_id: _Optional[str] = ...) -> None: ...

class ReloadCredentialsRequest(_message.Message):
    __slots__ = ("credential_generation", "config_checksum", "workspace_key", "runner_generation")
    CREDENTIAL_GENERATION_FIELD_NUMBER: _ClassVar[int]
    CONFIG_CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_KEY_FIELD_NUMBER: _ClassVar[int]
    RUNNER_GENERATION_FIELD_NUMBER: _ClassVar[int]
    credential_generation: int
    config_checksum: str
    workspace_key: str
    runner_generation: int
    def __init__(self, credential_generation: _Optional[int] = ..., config_checksum: _Optional[str] = ..., workspace_key: _Optional[str] = ..., runner_generation: _Optional[int] = ...) -> None: ...

class ReloadCredentialsResponse(_message.Message):
    __slots__ = ("credential_generation", "config_checksum")
    CREDENTIAL_GENERATION_FIELD_NUMBER: _ClassVar[int]
    CONFIG_CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    credential_generation: int
    config_checksum: str
    def __init__(self, credential_generation: _Optional[int] = ..., config_checksum: _Optional[str] = ...) -> None: ...

class TaskEnvelope(_message.Message):
    __slots__ = ("task_id", "session_key", "requester_user_id", "source", "source_instance_id", "message_id", "prompt", "persona_snapshot", "tool_policy_version", "created_at", "runner_generation", "capability_jti")
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    SESSION_KEY_FIELD_NUMBER: _ClassVar[int]
    REQUESTER_USER_ID_FIELD_NUMBER: _ClassVar[int]
    SOURCE_FIELD_NUMBER: _ClassVar[int]
    SOURCE_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_ID_FIELD_NUMBER: _ClassVar[int]
    PROMPT_FIELD_NUMBER: _ClassVar[int]
    PERSONA_SNAPSHOT_FIELD_NUMBER: _ClassVar[int]
    TOOL_POLICY_VERSION_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    RUNNER_GENERATION_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_JTI_FIELD_NUMBER: _ClassVar[int]
    task_id: str
    session_key: str
    requester_user_id: int
    source: str
    source_instance_id: str
    message_id: str
    prompt: str
    persona_snapshot: _containers.RepeatedScalarFieldContainer[str]
    tool_policy_version: str
    created_at: _timestamp_pb2.Timestamp
    runner_generation: int
    capability_jti: str
    def __init__(self, task_id: _Optional[str] = ..., session_key: _Optional[str] = ..., requester_user_id: _Optional[int] = ..., source: _Optional[str] = ..., source_instance_id: _Optional[str] = ..., message_id: _Optional[str] = ..., prompt: _Optional[str] = ..., persona_snapshot: _Optional[_Iterable[str]] = ..., tool_policy_version: _Optional[str] = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., runner_generation: _Optional[int] = ..., capability_jti: _Optional[str] = ...) -> None: ...

class ExecuteTaskRequest(_message.Message):
    __slots__ = ("task",)
    TASK_FIELD_NUMBER: _ClassVar[int]
    task: TaskEnvelope
    def __init__(self, task: _Optional[_Union[TaskEnvelope, _Mapping]] = ...) -> None: ...

class Chunk(_message.Message):
    __slots__ = ("task_id", "text", "turn")
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    TEXT_FIELD_NUMBER: _ClassVar[int]
    TURN_FIELD_NUMBER: _ClassVar[int]
    task_id: str
    text: str
    turn: int
    def __init__(self, task_id: _Optional[str] = ..., text: _Optional[str] = ..., turn: _Optional[int] = ...) -> None: ...

class ToolProgress(_message.Message):
    __slots__ = ("task_id", "text", "turn")
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    TEXT_FIELD_NUMBER: _ClassVar[int]
    TURN_FIELD_NUMBER: _ClassVar[int]
    task_id: str
    text: str
    turn: int
    def __init__(self, task_id: _Optional[str] = ..., text: _Optional[str] = ..., turn: _Optional[int] = ...) -> None: ...

class BeginCheckpointRequest(_message.Message):
    __slots__ = ("task_id", "checkpoint_token", "staging_ref", "max_bundle_bytes", "runner_generation", "capability_jti")
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_TOKEN_FIELD_NUMBER: _ClassVar[int]
    STAGING_REF_FIELD_NUMBER: _ClassVar[int]
    MAX_BUNDLE_BYTES_FIELD_NUMBER: _ClassVar[int]
    RUNNER_GENERATION_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_JTI_FIELD_NUMBER: _ClassVar[int]
    task_id: str
    checkpoint_token: str
    staging_ref: str
    max_bundle_bytes: int
    runner_generation: int
    capability_jti: str
    def __init__(self, task_id: _Optional[str] = ..., checkpoint_token: _Optional[str] = ..., staging_ref: _Optional[str] = ..., max_bundle_bytes: _Optional[int] = ..., runner_generation: _Optional[int] = ..., capability_jti: _Optional[str] = ...) -> None: ...

class CheckpointReady(_message.Message):
    __slots__ = ("task_id", "checkpoint_token", "staging_ref", "checksum", "result_digest", "runner_generation")
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_TOKEN_FIELD_NUMBER: _ClassVar[int]
    STAGING_REF_FIELD_NUMBER: _ClassVar[int]
    CHECKSUM_FIELD_NUMBER: _ClassVar[int]
    RESULT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RUNNER_GENERATION_FIELD_NUMBER: _ClassVar[int]
    task_id: str
    checkpoint_token: str
    staging_ref: str
    checksum: str
    result_digest: str
    runner_generation: int
    def __init__(self, task_id: _Optional[str] = ..., checkpoint_token: _Optional[str] = ..., staging_ref: _Optional[str] = ..., checksum: _Optional[str] = ..., result_digest: _Optional[str] = ..., runner_generation: _Optional[int] = ...) -> None: ...

class ErrorEnvelope(_message.Message):
    __slots__ = ("code", "user_message", "trace_id")
    CODE_FIELD_NUMBER: _ClassVar[int]
    USER_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    TRACE_ID_FIELD_NUMBER: _ClassVar[int]
    code: str
    user_message: str
    trace_id: str
    def __init__(self, code: _Optional[str] = ..., user_message: _Optional[str] = ..., trace_id: _Optional[str] = ...) -> None: ...

class Terminal(_message.Message):
    __slots__ = ("task_id", "status", "result_digest", "user_message", "error")
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    RESULT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    USER_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    task_id: str
    status: TerminalStatus
    result_digest: str
    user_message: str
    error: ErrorEnvelope
    def __init__(self, task_id: _Optional[str] = ..., status: _Optional[_Union[TerminalStatus, str]] = ..., result_digest: _Optional[str] = ..., user_message: _Optional[str] = ..., error: _Optional[_Union[ErrorEnvelope, _Mapping]] = ...) -> None: ...

class WorkerEvent(_message.Message):
    __slots__ = ("chunk", "tool_progress", "terminal")
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    TOOL_PROGRESS_FIELD_NUMBER: _ClassVar[int]
    TERMINAL_FIELD_NUMBER: _ClassVar[int]
    chunk: Chunk
    tool_progress: ToolProgress
    terminal: Terminal
    def __init__(self, chunk: _Optional[_Union[Chunk, _Mapping]] = ..., tool_progress: _Optional[_Union[ToolProgress, _Mapping]] = ..., terminal: _Optional[_Union[Terminal, _Mapping]] = ...) -> None: ...

class CancelTaskRequest(_message.Message):
    __slots__ = ("task_id", "workspace_key", "runner_generation", "capability_jti")
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_KEY_FIELD_NUMBER: _ClassVar[int]
    RUNNER_GENERATION_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_JTI_FIELD_NUMBER: _ClassVar[int]
    task_id: str
    workspace_key: str
    runner_generation: int
    capability_jti: str
    def __init__(self, task_id: _Optional[str] = ..., workspace_key: _Optional[str] = ..., runner_generation: _Optional[int] = ..., capability_jti: _Optional[str] = ...) -> None: ...

class CancelTaskResponse(_message.Message):
    __slots__ = ("accepted",)
    ACCEPTED_FIELD_NUMBER: _ClassVar[int]
    accepted: bool
    def __init__(self, accepted: _Optional[bool] = ...) -> None: ...

class HealthRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class HealthResponse(_message.Message):
    __slots__ = ("worker_instance_id", "session_key", "ready")
    WORKER_INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    SESSION_KEY_FIELD_NUMBER: _ClassVar[int]
    READY_FIELD_NUMBER: _ClassVar[int]
    worker_instance_id: str
    session_key: str
    ready: bool
    def __init__(self, worker_instance_id: _Optional[str] = ..., session_key: _Optional[str] = ..., ready: _Optional[bool] = ...) -> None: ...

class ShutdownRequest(_message.Message):
    __slots__ = ("reason", "workspace_key", "runner_generation", "capability_jti")
    REASON_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_KEY_FIELD_NUMBER: _ClassVar[int]
    RUNNER_GENERATION_FIELD_NUMBER: _ClassVar[int]
    CAPABILITY_JTI_FIELD_NUMBER: _ClassVar[int]
    reason: str
    workspace_key: str
    runner_generation: int
    capability_jti: str
    def __init__(self, reason: _Optional[str] = ..., workspace_key: _Optional[str] = ..., runner_generation: _Optional[int] = ..., capability_jti: _Optional[str] = ...) -> None: ...

class ShutdownResponse(_message.Message):
    __slots__ = ("accepted",)
    ACCEPTED_FIELD_NUMBER: _ClassVar[int]
    accepted: bool
    def __init__(self, accepted: _Optional[bool] = ...) -> None: ...
