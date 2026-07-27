import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Badge } from '../../components/ui/Badge';
import { Check, X } from 'lucide-react';
import { listPendingPersonas, approvePersona, rejectPersona } from '../../api/personas';
import { ApiClientError } from '../../api/client';
import type { Persona } from '../../api/types';
import './AdminPages.css';

export function PersonaReviewPage() {
  const [personas, setPersonas] = useState<Persona[]>([]);
  const [notes, setNotes] = useState<Record<string, string>>({});
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  const loadPersonas = async () => {
    setIsLoading(true);
    setError('');
    try {
      const list = await listPendingPersonas();
      setPersonas(list);
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void listPendingPersonas()
      .then((list) => {
        if (active) setPersonas(list);
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const handleModerate = async (id: string, approve: boolean) => {
    setError('');
    try {
      if (approve) {
        await approvePersona(id, notes[id] || '');
      } else {
        await rejectPersona(id, notes[id] || '');
      }
      await loadPersonas();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '审核失败');
    }
  };

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>人设审核</h1>
          <p className="admin-subtitle">审核用户提交到公共商店的人设</p>
        </div>
        <Button variant="secondary" onClick={loadPersonas} disabled={isLoading}>刷新</Button>
      </header>

      <Card className="animate-fade-in-up animate-delay-1">
        {error && <span className="input-error" style={{ display: 'block', marginBottom: '12px' }}>{error}</span>}
        {isLoading ? (
          <p className="admin-empty">加载中...</p>
        ) : personas.length === 0 ? (
          <p className="admin-empty">暂无待审核人设</p>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: '20px' }}>
            {personas.map((p) => (
              <div
                key={p.id}
                style={{
                  padding: '20px',
                  background: 'var(--bg-raised)',
                  border: '1px solid var(--border)',
                  borderRadius: 'var(--radius-sm)',
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                    <strong style={{ fontSize: '16px' }}>{p.name}</strong>
                    <Badge variant="warning">审核中</Badge>
                    <span className="admin-subtitle">作者: {p.author_id}</span>
                  </div>
                  <div className="admin-actions">
                    <Button variant="danger" onClick={() => handleModerate(p.id, false)}>
                      <X size={14} />
                      拒绝
                    </Button>
                    <Button onClick={() => handleModerate(p.id, true)}>
                      <Check size={14} />
                      批准
                    </Button>
                  </div>
                </div>
                {p.description && (
                  <p className="page-subtitle" style={{ marginTop: '10px' }}>{p.description}</p>
                )}
                <pre style={{ marginTop: '12px', padding: '12px', background: 'var(--bg-overlay)', borderRadius: 'var(--radius-sm)', fontSize: '13px', whiteSpace: 'pre-wrap' }}>
                  {p.system_prompt}
                </pre>
                <input
                  type="text"
                  placeholder="审核备注（可选）"
                  value={notes[p.id] || ''}
                  onChange={(e) => setNotes((prev) => ({ ...prev, [p.id]: e.target.value }))}
                  style={{
                    width: '100%',
                    marginTop: '12px',
                    padding: '10px',
                    background: 'var(--bg-overlay)',
                    border: '1px solid var(--border)',
                    borderRadius: 'var(--radius-sm)',
                    color: 'var(--text-primary)',
                  }}
                />
              </div>
            ))}
          </div>
        )}
      </Card>
    </div>
  );
}
