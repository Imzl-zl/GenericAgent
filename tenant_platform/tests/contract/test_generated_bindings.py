import sys
from pathlib import Path

ROOT = Path(__file__).parents[2]
SRC = ROOT / "worker-python" / "src"
if str(SRC) not in sys.path:
    sys.path.insert(0, str(SRC))


def test_worker_generated_bindings_expose_required_service_surface():
    from genericagent.worker.v1 import worker_pb2, worker_pb2_grpc

    service = worker_pb2.DESCRIPTOR.services_by_name["WorkerService"]
    method_names = {method.name for method in service.methods}
    assert method_names == {
        "StartSession",
        "ExecuteTask",
        "BeginCheckpoint",
        "CancelTask",
        "Health",
        "Shutdown",
    }
    assert worker_pb2.DESCRIPTOR.package == "genericagent.worker.v1"
    assert hasattr(worker_pb2_grpc, "WorkerServiceServicer")
    assert hasattr(worker_pb2_grpc, "WorkerServiceStub")
