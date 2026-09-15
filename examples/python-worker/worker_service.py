from app import build_worker
from naatre.worker_stdio import serve_stdio

if __name__ == "__main__":
    serve_stdio(build_worker())
