"""gRPC WorkerService servicer delegating to ManagedAgentAdapter."""

from __future__ import annotations

import grpc

from genericagent.worker.v1 import worker_pb2, worker_pb2_grpc
from ga_worker.managed_agent import ManagedAgentAdapter, WorkerAdapterError


def _abort(context: grpc.ServicerContext, exc: WorkerAdapterError) -> None:
    # Surface structured code in details; status is FAILED_PRECONDITION for domain errors.
    context.set_code(grpc.StatusCode.FAILED_PRECONDITION)
    context.set_details(f"{exc.code}: {exc.message}")
    # Also attach trailing metadata for clients that inspect code.
    context.set_trailing_metadata((("error-code", exc.code), ("error-message", exc.message[:200])))


class WorkerServicer(worker_pb2_grpc.WorkerServiceServicer):
    def __init__(self, adapter: ManagedAgentAdapter):
        self._adapter = adapter

    def StartSession(self, request, context):
        try:
            return self._adapter.start_session(request)
        except WorkerAdapterError as exc:
            _abort(context, exc)
            return worker_pb2.StartSessionResponse()

    def ReloadCredentials(self, request, context):
        try:
            return self._adapter.reload_credentials(request)
        except WorkerAdapterError as exc:
            _abort(context, exc)
            return worker_pb2.ReloadCredentialsResponse()

    def ExecuteTask(self, request, context):
        try:
            for event in self._adapter.execute_task(request):
                yield event
        except WorkerAdapterError as exc:
            _abort(context, exc)
            return

    def BeginCheckpoint(self, request, context):
        try:
            return self._adapter.begin_checkpoint(request)
        except WorkerAdapterError as exc:
            _abort(context, exc)
            return worker_pb2.CheckpointReady()

    def CancelTask(self, request, context):
        try:
            return self._adapter.cancel_task(
                request.task_id, request.workspace_key, request.runner_generation
            )
        except WorkerAdapterError as exc:
            _abort(context, exc)
            return worker_pb2.CancelTaskResponse(accepted=False)

    def Health(self, request, context):
        try:
            return self._adapter.health()
        except WorkerAdapterError as exc:
            _abort(context, exc)
            return worker_pb2.HealthResponse()

    def Shutdown(self, request, context):
        try:
            return self._adapter.shutdown(
                request.reason or "", request.workspace_key, request.runner_generation
            )
        except WorkerAdapterError as exc:
            _abort(context, exc)
            return worker_pb2.ShutdownResponse(accepted=False)


def add_worker_servicer(server: grpc.Server, adapter: ManagedAgentAdapter) -> None:
    worker_pb2_grpc.add_WorkerServiceServicer_to_server(WorkerServicer(adapter), server)
