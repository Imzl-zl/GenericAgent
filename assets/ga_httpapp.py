import threading, sys, os
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
from fastapi import FastAPI, Header, HTTPException, Query
from pydantic import BaseModel
from agentmain import GeneraticAgent

PORT, API_KEY = int(sys.argv[1]), sys.argv[2]
app, agent, lock = FastAPI(), GeneraticAgent(), threading.Lock()
task_id, outputs, stopped, base = 0, [], True, 0
threading.Thread(target=agent.run, daemon=True).start()
class Req(BaseModel): prompt: str
agent.verbose = False

def auth(key):
    if key != API_KEY: raise HTTPException(404)

def run_task(tid, prompt):
    global stopped
    try:
        dq = agent.put_task(prompt, source="http")
        while "done" not in (item := dq.get(timeout=1200)):
            if "next" in item:
                t = base + item.get("turn", 0)
                while len(outputs) <= t: outputs.append({"task_id": tid, "text": ""})
                outputs[t] = {"task_id": tid, "text": item["outputs"][-1] if item.get("outputs") else ""}
        for i, txt in enumerate(item.get("outputs", [])):
            t = base + i
            while len(outputs) <= t: outputs.append({"task_id": tid, "text": ""})
            outputs[t] = {"task_id": tid, "text": txt}
    finally: stopped = True

@app.post("/put_task")
def put_task(req: Req, key: str = Header(alias="X-API-Key")):
    auth(key); global task_id, stopped, base
    with lock:
        if not stopped: return {"ok": False, "error": "should abort first"}
        stopped = False; task_id += 1; base = len(outputs)
    threading.Thread(target=run_task, args=(task_id, req.prompt), daemon=True).start()
    return {"ok": True, "task_id": task_id}

@app.post("/abort")
def abort(key: str = Header(alias="X-API-Key")): auth(key); agent.abort(); return {"ok": True}

@app.get("/output")
def get_output(key: str = Header(alias="X-API-Key"), k: int = Query(5)):
    auth(key)
    recent = [o["text"] for o in outputs[-k:]] if outputs else []
    return {"task_id": task_id, "stopped": stopped, "output": "\n".join(recent)}

if __name__ == "__main__":
    import uvicorn; uvicorn.run(app, host="0.0.0.0", port=PORT)
