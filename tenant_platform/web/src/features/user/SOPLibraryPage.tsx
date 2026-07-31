import { useEffect, useState } from 'react';
import { FileCheck2, RefreshCw } from 'lucide-react';
import { ApiClientError } from '../../api/client';
import { listLoadedSOPs, type LoadedSOP } from '../../api/sops';
import { Badge } from '../../components/ui/Badge';
import { Button } from '../../components/ui/Button';
import { Card } from '../../components/ui/Card';
import './UserPages.css';

const shortDigest = (digest: string) => `${digest.slice(0, 12)}...${digest.slice(-8)}`;

export function SOPLibraryPage() {
  const [sops, setSOPs] = useState<LoadedSOP[]>([]);
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  const load = async () => {
    setIsLoading(true);
    try {
      setSOPs(await listLoadedSOPs());
      setError('');
    } catch (loadError) {
      setError(loadError instanceof ApiClientError ? `${loadError.code}: ${loadError.message}` : '加载 SOP 失败');
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void listLoadedSOPs()
      .then((items) => {
        if (!active) return;
        setSOPs(items);
        setError('');
      })
      .catch((loadError: unknown) => {
        if (!active) return;
        setError(loadError instanceof ApiClientError ? `${loadError.code}: ${loadError.message}` : '加载 SOP 失败');
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  return (
    <div className="page sop-library-page">
      <header className="page-header animate-fade-in-up">
        <div>
          <h1>已加载 SOP</h1>
          <p className="page-subtitle">管理员批准并用于新任务的标准作业流程</p>
        </div>
        <Button variant="secondary" onClick={() => void load()} disabled={isLoading}><RefreshCw size={15} />刷新</Button>
      </header>

      {error && <div className="sop-library-error" role="alert">{error}</div>}
      {isLoading ? <p className="empty-cell">加载中...</p> : sops.length === 0 ? (
        <div className="sop-library-empty"><FileCheck2 size={24} /><p>当前没有已加载 SOP</p></div>
      ) : (
        <div className="sop-library-list">
          {sops.map((sop) => (
            <Card key={`${sop.digest}:${sop.version}`} className="sop-library-item animate-fade-in-up">
              <div className="sop-library-heading">
                <div><h3>{sop.title}</h3>{sop.description && <p>{sop.description}</p>}</div>
                <Badge variant="success">{`V${sop.version}`}</Badge>
              </div>
              <details>
                <summary>查看 SOP 正文</summary>
                <pre>{sop.content}</pre>
              </details>
              <code title={sop.digest}>{shortDigest(sop.digest)}</code>
            </Card>
          ))}
        </div>
      )}
    </div>
  );
}
