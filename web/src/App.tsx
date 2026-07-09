import React, { useEffect, useState } from 'react';

interface Configs {
  module_emotion_tracker: string;
  module_graph_self_evolution: string;
  module_multi_turn_graph: string;
  module_image_dedup: string;
  system_prompt: string;
  user_name: string;
  bot_name: string;
  partner_name: string;
  init_completed: string;
}

interface TrendPoint {
  timestamp: number;
  mood: number;
  affinity: number;
  sentiment: string;
}

interface Trio {
  src: string;
  relation: string;
  dst: string;
}

interface DBStatus {
  status: string;
  version: string;
  max_open_conns: number;
  open_conns: number;
  in_use_conns: number;
  idle_conns: number;
  wait_count: number;
  entity_count: number;
  relation_count: number;
  profile_id: string;
  error?: string;
}

function App() {
  const [configs, setConfigs] = useState<Configs>({
    module_emotion_tracker: 'true',
    module_graph_self_evolution: 'true',
    module_multi_turn_graph: 'true',
    module_image_dedup: 'true',
    system_prompt: '',
    user_name: '',
    bot_name: '',
    partner_name: '',
    init_completed: 'false',
  });

  const [trends, setTrends] = useState<TrendPoint[]>([]);
  const [trios, setTrios] = useState<Trio[]>([]);
  const [dbStatus, setDbStatus] = useState<DBStatus>({
    status: 'offline',
    version: 'Unknown',
    max_open_conns: 0,
    open_conns: 0,
    in_use_conns: 0,
    idle_conns: 0,
    wait_count: 0,
    entity_count: 0,
    relation_count: 0,
    profile_id: 'unknown',
  });

  // Triplet input fields
  const [newSrc, setNewSrc] = useState('');
  const [newRelation, setNewRelation] = useState('别名');
  const [newDst, setNewDst] = useState('');

  // Onboarding Wizard Fields
  const [wizardUser, setWizardUser] = useState('');
  const [wizardBot, setWizardBot] = useState('');
  const [wizardPartner, setWizardPartner] = useState('');
  const [wizardPrompt, setWizardPrompt] = useState('');

  // Auto detect port for local developer proxy bypass
  const getBaseUrl = () => {
    if (window.location.port === '5173') {
      return 'http://127.0.0.1:8080';
    }
    return '';
  };

  const API_BASE = getBaseUrl();

  const fetchConfigs = async () => {
    try {
      const res = await fetch(`${API_BASE}/api/configs`);
      const data = await res.json();
      setConfigs(data);
      if (data.init_completed === 'true') {
        // sync wizard fields from loaded config
        setWizardUser(data.user_name || '');
        setWizardBot(data.bot_name || '');
        setWizardPartner(data.partner_name || '');
        setWizardPrompt(data.system_prompt || '');
      }
    } catch (e) {
      console.error('Failed to fetch configs', e);
    }
  };

  const fetchTrends = async () => {
    try {
      const res = await fetch(`${API_BASE}/api/emotion-trends`);
      const data = await res.json();
      setTrends(data);
    } catch (e) {
      console.error('Failed to fetch trends', e);
    }
  };

  const fetchTrios = async () => {
    try {
      const res = await fetch(`${API_BASE}/api/graph`);
      const data = await res.json();
      setTrios(data);
    } catch (e) {
      console.error('Failed to fetch graph data', e);
    }
  };

  const fetchDbStatus = async () => {
    try {
      const res = await fetch(`${API_BASE}/api/db-status`);
      const data = await res.json();
      setDbStatus(data);
    } catch (e) {
      console.error('Failed to fetch db status', e);
      setDbStatus(prev => ({ ...prev, status: 'offline' }));
    }
  };

  useEffect(() => {
    fetchConfigs();
    fetchTrends();
    fetchTrios();
    fetchDbStatus();

    const timer = setInterval(() => {
      fetchDbStatus();
      fetchTrends();
    }, 5000);

    return () => clearInterval(timer);
  }, []);

  const handleToggle = async (key: keyof Configs) => {
    const nextValue = configs[key] === 'true' ? 'false' : 'true';
    setConfigs(prev => ({ ...prev, [key]: nextValue }));

    try {
      await fetch(`${API_BASE}/api/configs`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key, value: nextValue }),
      });
    } catch (e) {
      console.error('Failed to update config', e);
      setConfigs(prev => ({ ...prev, [key]: configs[key] }));
    }
  };

  const handleOnboardingSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!wizardUser.trim() || !wizardBot.trim() || !wizardPrompt.trim()) {
      alert('请完整填写主人称呼、机器人称呼以及系统提示词！');
      return;
    }

    const payload = {
      configs: {
        user_name: wizardUser.trim(),
        bot_name: wizardBot.trim(),
        partner_name: wizardPartner.trim(),
        system_prompt: wizardPrompt.trim(),
        init_completed: 'true',
      }
    };

    try {
      const res = await fetch(`${API_BASE}/api/configs`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (res.ok) {
        // Automatically inject alias rules for user/partner on onboarding complete
        await fetch(`${API_BASE}/api/graph`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ src: wizardBot.trim(), relation: '别名', dst: '机器人' })
        });
        if (wizardPartner.trim()) {
          await fetch(`${API_BASE}/api/graph`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ src: wizardPartner.trim(), relation: '别名', dst: '女主角' })
          });
        }
        fetchConfigs();
        fetchTrios();
        fetchDbStatus();
      }
    } catch (e) {
      console.error('Failed to submit onboarding', e);
    }
  };

  const loadDefaultTemplate = () => {
    const userName = wizardUser || '三哥';
    const botName = wizardBot || '小弟';
    const partnerName = wizardPartner || '舒舒';
    setWizardPrompt(
      `你是${botName}，${userName}的小助手。${partnerName ? `和${partnerName}关系亲密。` : ''}你不是主导人，语气轻松、幽默自然，偶尔皮一下但克制不腻。分清发言人身份。不要说多余的解释。`
    );
  };

  const handleAddTriplet = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newSrc.trim() || !newRelation.trim() || !newDst.trim()) return;

    try {
      const res = await fetch(`${API_BASE}/api/graph`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          src: newSrc.trim(),
          relation: newRelation.trim(),
          dst: newDst.trim(),
        }),
      });
      if (res.ok) {
        setNewSrc('');
        setNewDst('');
        fetchTrios();
        fetchDbStatus();
      }
    } catch (e) {
      console.error('Failed to add triplet', e);
    }
  };

  const handleDeleteTriplet = async (trio: Trio) => {
    if (!window.confirm(`确定要从图谱中物理删除关系: (${trio.src}, ${trio.relation}, ${trio.dst}) 吗？`)) {
      return;
    }

    try {
      const res = await fetch(`${API_BASE}/api/graph`, {
        method: 'DELETE',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(trio),
      });
      if (res.ok) {
        fetchTrios();
        fetchDbStatus();
      }
    } catch (e) {
      console.error('Failed to delete triplet', e);
    }
  };

  const renderSVGChart = () => {
    if (trends.length === 0) {
      return <div className="empty-state">暂无情感亲密度温度计数据，机器人需要先进行聊天会话</div>;
    }

    const width = 500;
    const height = 200;
    const padding = 25;

    const getX = (index: number) => {
      if (trends.length <= 1) return width / 2;
      return padding + (index * (width - 2 * padding)) / (trends.length - 1);
    };

    const getY = (score: number) => {
      return height - padding - (score * (height - 2 * padding)) / 100;
    };

    let moodPoints = '';
    let affinityPoints = '';
    let moodArea = `M ${getX(0)} ${height - padding} `;
    let affinityArea = `M ${getX(0)} ${height - padding} `;

    trends.forEach((pt, index) => {
      const x = getX(index);
      const yMood = getY(pt.mood);
      const yAff = getY(pt.affinity);

      if (index === 0) {
        moodPoints = `M ${x} ${yMood}`;
        affinityPoints = `M ${x} ${yAff}`;
      } else {
        moodPoints += ` L ${x} ${yMood}`;
        affinityPoints += ` L ${x} ${yAff}`;
      }

      moodArea += `L ${x} ${yMood} `;
      affinityArea += `L ${x} ${yAff} `;
    });

    moodArea += `L ${getX(trends.length - 1)} ${height - padding} Z`;
    affinityArea += `L ${getX(trends.length - 1)} ${height - padding} Z`;

    const formatTime = (ts: number) => {
      const d = new Date(ts * 1000);
      return `${d.getMonth() + 1}-${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
    };

    return (
      <svg viewBox={`0 0 ${width} ${height}`} width="100%" height="100%">
        <defs>
          <linearGradient id="moodGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="#ff4983" stopOpacity="0.25" />
            <stop offset="100%" stopColor="#ff4983" stopOpacity="0.0" />
          </linearGradient>
          <linearGradient id="affGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="#00b4ff" stopOpacity="0.25" />
            <stop offset="100%" stopColor="#00b4ff" stopOpacity="0.0" />
          </linearGradient>
        </defs>

        {[20, 50, 80, 100].map(val => (
          <line
            key={val}
            x1={padding}
            y1={getY(val)}
            x2={width - padding}
            y2={getY(val)}
            stroke="rgba(255, 255, 255, 0.03)"
            strokeWidth="1"
          />
        ))}

        <path d={moodArea} fill="url(#moodGrad)" />
        <path d={affinityArea} fill="url(#affGrad)" />

        <path d={moodPoints} fill="none" stroke="#ff4983" strokeWidth="2.5" strokeLinecap="round" />
        <path d={affinityPoints} fill="none" stroke="#00b4ff" strokeWidth="2.5" strokeLinecap="round" />

        {trends.map((pt, idx) => (
          <g key={idx}>
            <circle cx={getX(idx)} cy={getY(pt.mood)} r="4" fill="#ff4983" stroke="#080a13" strokeWidth="1.5" />
            <circle cx={getX(idx)} cy={getY(pt.affinity)} r="4" fill="#00b4ff" stroke="#080a13" strokeWidth="1.5" />
            {idx % Math.max(1, Math.floor(trends.length / 5)) === 0 && (
              <text x={getX(idx)} y={height - 5} fill="#6b7280" fontSize="7.5" textAnchor="middle">
                {formatTime(pt.timestamp)}
              </text>
            )}
          </g>
        ))}
      </svg>
    );
  };

  const getPoolUsagePercentage = () => {
    if (dbStatus.max_open_conns <= 0) return 0;
    return (dbStatus.in_use_conns / dbStatus.max_open_conns) * 100;
  };

  // Render Onboarding Screen if the bot setup hasn't been initialized
  if (configs.init_completed !== 'true') {
    return (
      <div className="app-container" style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', padding: '20px' }}>
        <div className="glass-card onboarding-card" style={{ maxWidth: '600px', width: '100%', padding: '40px' }}>
          <div style={{ textAlign: 'center', marginBottom: '30px' }}>
            <h1 style={{ fontSize: '2rem', marginBottom: '10px', background: 'var(--primary-gradient)', WebkitBackgroundClip: 'text', WebkitTextFillColor: 'transparent' }}>
              初始化飞书陪伴小弟
            </h1>
            <p style={{ color: 'var(--text-muted)' }}>设定角色人设与专属称呼，零门槛快速接入您的专属 Bot 体验</p>
          </div>

          <form onSubmit={handleOnboardingSubmit} style={{ display: 'flex', flexDirection: 'column', gap: '20px' }}>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '15px' }}>
              <div>
                <label style={{ display: 'block', marginBottom: '8px', fontSize: '0.85rem', color: 'var(--text-muted)' }}>主人的称呼 (User Name) *</label>
                <input
                  type="text"
                  placeholder="如 三哥"
                  value={wizardUser}
                  onChange={e => setWizardUser(e.target.value)}
                  style={{ width: '100%' }}
                  required
                />
              </div>
              <div>
                <label style={{ display: 'block', marginBottom: '8px', fontSize: '0.85rem', color: 'var(--text-muted)' }}>机器人的称呼 (Bot Name) *</label>
                <input
                  type="text"
                  placeholder="如 小弟"
                  value={wizardBot}
                  onChange={e => setWizardBot(e.target.value)}
                  style={{ width: '100%' }}
                  required
                />
              </div>
            </div>

            <div>
              <label style={{ display: 'block', marginBottom: '8px', fontSize: '0.85rem', color: 'var(--text-muted)' }}>陪伴角色名字 (Partner Name - 可空)</label>
              <input
                type="text"
                placeholder="如 舒舒 (用于消解图谱与关系关联)"
                value={wizardPartner}
                onChange={e => setWizardPartner(e.target.value)}
                style={{ width: '100%' }}
              />
            </div>

            <div>
              <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '8px' }}>
                <label style={{ fontSize: '0.85rem', color: 'var(--text-muted)' }}>自定义系统人设提示词 (System Prompt) *</label>
                <button
                  type="button"
                  onClick={loadDefaultTemplate}
                  style={{ background: 'none', border: 'none', color: '#a235ff', cursor: 'pointer', fontSize: '0.82rem', padding: 0 }}
                >
                  ⚡ 装载推荐模板
                </button>
              </div>
              <textarea
                placeholder="请定义机器人的性格特征、回复语气、以及它的世界观设定..."
                value={wizardPrompt}
                onChange={e => setWizardPrompt(e.target.value)}
                rows={6}
                style={{
                  width: '100%',
                  background: 'rgba(0, 0, 0, 0.2)',
                  border: '1px solid rgba(255, 255, 255, 0.08)',
                  borderRadius: '10px',
                  padding: '12px',
                  color: 'white',
                  fontFamily: 'inherit',
                  fontSize: '0.9rem',
                  resize: 'vertical',
                }}
                required
              />
            </div>

            <button type="submit" className="btn" style={{ width: '100%', marginTop: '10px' }}>
              完成人设并开启仪表盘
            </button>
          </form>
        </div>
      </div>
    );
  }

  // Render Dashboard
  return (
    <div className="app-container">
      <header>
        <div className="logo-section">
          <h1>Feishu Companion Bot</h1>
          <p>小弟机器人控制台 & 陪伴情感看板</p>
        </div>
        <div style={{ display: 'flex', gap: '10px', alignItems: 'center' }}>
          <span className="badge">长连接正常运行中</span>
          <button
            onClick={() => {
              if (window.confirm('重置人设将会返回到向导页，您需要重新填写，确认吗？')) {
                handleToggle('init_completed');
              }
            }}
            style={{
              background: 'rgba(255, 255, 255, 0.05)',
              border: '1px solid rgba(255, 255, 255, 0.08)',
              color: 'var(--text-muted)',
              padding: '6px 12px',
              borderRadius: '8px',
              cursor: 'pointer',
              fontSize: '0.8rem'
            }}
          >
            ⚙️ 重置角色引导
          </button>
        </div>
      </header>

      <div className="dashboard-grid">
        {/* Left Column: Settings & Database Panel */}
        <div style={{ display: 'flex', flexDirection: 'column', gap: '30px' }}>
          {/* Card 1: Modular Config Toggles */}
          <div className="glass-card">
            <h2 className="card-title">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.1a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/></svg>
              智能模块热配置
            </h2>
            <div className="settings-list">
              <div className="setting-item">
                <div className="setting-info">
                  <h4>情感亲密度温度计 (Emotion Tracker)</h4>
                  <p>实时分析主人的言语态度，动态调整关系评分与对话人设</p>
                </div>
                <label className="switch">
                  <input
                    type="checkbox"
                    checked={configs.module_emotion_tracker === 'true'}
                    onChange={() => handleToggle('module_emotion_tracker')}
                  />
                  <span className="slider"></span>
                </label>
              </div>

              <div className="setting-item">
                <div className="setting-info">
                  <h4>图谱冲突演进与纠偏 (Self-Evolution)</h4>
                  <p>自动识别逻辑冲突，由 LLM 主动对旧关系进行删除/覆盖</p>
                </div>
                <label className="switch">
                  <input
                    type="checkbox"
                    checked={configs.module_graph_self_evolution === 'true'}
                    onChange={() => handleToggle('module_graph_self_evolution')}
                  />
                  <span className="slider"></span>
                </label>
              </div>

              <div className="setting-item">
                <div className="setting-info">
                  <h4>跨会话上下文推理 (Multi-turn GraphRAG)</h4>
                  <p>提炼图谱时融合最近对话上下文历史，消解隐式代词指代</p>
                </div>
                <label className="switch">
                  <input
                    type="checkbox"
                    checked={configs.module_multi_turn_graph === 'true'}
                    onChange={() => handleToggle('module_multi_turn_graph')}
                  />
                  <span className="slider"></span>
                </label>
              </div>

              <div className="setting-item">
                <div className="setting-info">
                  <h4>表情包与图片秒懂 (Image Dedup Cache)</h4>
                  <p>运用 MD5 静默秒回复已识图片，免除大模型重复视觉提炼</p>
                </div>
                <label className="switch">
                  <input
                    type="checkbox"
                    checked={configs.module_image_dedup === 'true'}
                    onChange={() => handleToggle('module_image_dedup')}
                  />
                  <span className="slider"></span>
                </label>
              </div>
            </div>
          </div>

          {/* Card 2: OceanBase Monitor Status */}
          <div className="glass-card">
            <h2 className="card-title" style={{ justifyContent: 'space-between' }}>
              <span style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/><path d="M3 12c0 1.66 4 3 9 3s9-1.34 9-3"/></svg>
                OceanBase 数据库监控
              </span>
              <span style={{
                display: 'inline-flex',
                alignItems: 'center',
                gap: '6px',
                fontSize: '0.8rem',
                fontWeight: 600,
                color: dbStatus.status === 'online' ? '#10b981' : '#ef4444'
              }}>
                <span className={`status-dot ${dbStatus.status === 'online' ? 'online' : 'offline'}`} />
                {dbStatus.status === 'online' ? 'ONLINE' : 'OFFLINE'}
              </span>
            </h2>

            <div className="db-metrics-list" style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.85rem' }}>
                <span style={{ color: 'var(--text-muted)' }}>数据库版本</span>
                <span style={{ fontFamily: 'monospace', color: 'var(--text-main)' }}>{dbStatus.version}</span>
              </div>
              <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.85rem' }}>
                <span style={{ color: 'var(--text-muted)' }}>当前 Profile</span>
                <span style={{ fontWeight: 500 }}>{dbStatus.profile_id}</span>
              </div>

              {/* Pool utilization bar */}
              <div style={{ marginTop: '4px' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.82rem', marginBottom: '6px' }}>
                  <span style={{ color: 'var(--text-muted)' }}>连接池开销 (Active/Idle)</span>
                  <span style={{ fontWeight: 500 }}>
                    {dbStatus.in_use_conns} / {dbStatus.idle_conns} (Max: {dbStatus.max_open_conns})
                  </span>
                </div>
                <div className="progress-bar-bg">
                  <div
                    className="progress-bar-fill"
                    style={{ width: `${Math.max(4, getPoolUsagePercentage())}%` }}
                  />
                </div>
              </div>

              {/* Grid counters */}
              <div style={{
                display: 'grid',
                gridTemplateColumns: '1fr 1fr',
                gap: '12px',
                marginTop: '8px'
              }}>
                <div className="db-metric-box">
                  <div className="metric-num">{dbStatus.entity_count}</div>
                  <div className="metric-lbl">图谱实体 (Entities)</div>
                </div>
                <div className="db-metric-box">
                  <div className="metric-num">{dbStatus.relation_count}</div>
                  <div className="metric-lbl">关系数量 (Relations)</div>
                </div>
              </div>
            </div>
          </div>
        </div>

        {/* Right Column: Emotion Trends Chart */}
        <div className="glass-card" style={{ display: 'flex', flexDirection: 'column', justifyContent: 'space-between' }}>
          <div>
            <h2 className="card-title">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 6.86-8.55 11.54L12 21.35z"/></svg>
              恋爱亲密温度波动计
            </h2>
            <div className="chart-legend">
              <div className="legend-item">
                <span className="legend-color" style={{ backgroundColor: '#ff4983' }}></span>
                情绪分 (Mood)
              </div>
              <div className="legend-item">
                <span className="legend-color" style={{ backgroundColor: '#00b4ff' }}></span>
                好感度 (Affinity)
              </div>
            </div>
          </div>
          <div className="chart-container" style={{ flexGrow: 1, display: 'flex', alignItems: 'center' }}>
            {renderSVGChart()}
          </div>
        </div>

        {/* Bottom Column: Graph RAG Auditor */}
        <div className="glass-card graph-card">
          <h2 className="card-title">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M18 3a3 3 0 0 0-3 3v12a3 3 0 0 0 3 3 3 3 0 0 0 3-3V6a3 3 0 0 0-3-3zM6 21a3 3 0 0 0 3-3V6a3 3 0 0 0-3-3 3 3 0 0 0-3 3v12a3 3 0 0 0 3 3z"/></svg>
            Companion GraphRAG 关系图谱审计
          </h2>

          <form onSubmit={handleAddTriplet} className="triplet-form" style={{ gap: '10px', flexWrap: 'wrap' }}>
            <input
              type="text"
              placeholder="主语 (如 三哥)"
              value={newSrc}
              onChange={e => setNewSrc(e.target.value)}
              style={{ flex: 1, minWidth: '120px' }}
              required
            />
            
            {/* Custom Free-form input for relation with fast tags */}
            <div style={{ flex: 1, minWidth: '180px', display: 'flex', flexDirection: 'column', gap: '5px' }}>
              <input
                type="text"
                placeholder="关系边 (如 喜欢、别名、同事)"
                value={newRelation}
                onChange={e => setNewRelation(e.target.value)}
                style={{ width: '100%' }}
                required
              />
              <div className="quick-tags" style={{ display: 'flex', gap: '6px', overflowX: 'auto', padding: '2px 0' }}>
                {['别名', '喜欢', '所在地', '同事', '朋友', '妈妈', '爸爸'].map(t => (
                  <span
                    key={t}
                    onClick={() => setNewRelation(t)}
                    style={{
                      fontSize: '0.72rem',
                      background: newRelation === t ? 'rgba(162, 53, 255, 0.25)' : 'rgba(255, 255, 255, 0.05)',
                      border: `1px solid ${newRelation === t ? '#a235ff' : 'rgba(255, 255, 255, 0.08)'}`,
                      color: newRelation === t ? '#c58dff' : 'var(--text-muted)',
                      padding: '2px 8px',
                      borderRadius: '12px',
                      cursor: 'pointer',
                      whiteSpace: 'nowrap',
                      transition: 'all 0.2s'
                    }}
                  >
                    {t}
                  </span>
                ))}
              </div>
            </div>

            <input
              type="text"
              placeholder="宾语 (如 许君山)"
              value={newDst}
              onChange={e => setNewDst(e.target.value)}
              style={{ flex: 1, minWidth: '120px' }}
              required
            />
            <button type="submit" className="btn" style={{ minWidth: '100px' }}>
              添加边
            </button>
          </form>

          <div className="table-container" style={{ marginTop: '10px' }}>
            {trios.length === 0 ? (
              <div className="empty-state">图谱关系目前为空，可在上方手动添加，或等待聊天会话自动提取沉淀。</div>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>主语 (Subject)</th>
                    <th>关系边 (Predicate)</th>
                    <th>宾语 (Object)</th>
                    <th style={{ width: '80px', textAlign: 'center' }}>操作</th>
                  </tr>
                </thead>
                <tbody>
                  {trios.map((trio, idx) => (
                    <tr key={idx}>
                      <td style={{ fontWeight: 500 }}>{trio.src}</td>
                      <td>
                        <span style={{ fontFamily: 'monospace', color: '#ff6ea2', background: 'rgba(255, 110, 162, 0.08)', padding: '2px 8px', borderRadius: '4px', fontSize: '0.8rem' }}>
                          {trio.relation}
                        </span>
                      </td>
                      <td style={{ fontWeight: 500 }}>{trio.dst}</td>
                      <td style={{ textAlign: 'center' }}>
                        <button
                          onClick={() => handleDeleteTriplet(trio)}
                          className="btn-delete"
                        >
                          删除
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}

export default App;
