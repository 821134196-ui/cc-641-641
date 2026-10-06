// 极简 API 封装：统一解包 {data}，抛出含中文信息的错误
async function request(method, url, body) {
  const opt = { method, headers: { 'Content-Type': 'application/json' } }
  if (body !== undefined) opt.body = JSON.stringify(body)
  const res = await fetch(url, opt)
  let json = null
  try { json = await res.json() } catch { /* 非 JSON */ }
  if (!res.ok) {
    const msg = (json && json.error) || `请求失败 ${res.status}`
    const err = new Error(msg)
    err.status = res.status
    throw err
  }
  return json ? json.data : null
}

export const api = {
  health: () => request('GET', '/api/health'),

  listActivities: () => request('GET', '/api/activities'),
  createActivity: (b) => request('POST', '/api/activities', b),
  getActivity: (id) => request('GET', `/api/activities/${id}`),
  addPrize: (id, b) => request('POST', `/api/activities/${id}/prizes`, b),
  lock: (id) => request('POST', `/api/activities/${id}/lock`),
  end: (id) => request('POST', `/api/activities/${id}/end`),
  draw: (id) => request('POST', `/api/activities/${id}/draw`),
  result: (id) => request('GET', `/api/activities/${id}/result`),
  verifyHash: (id) => request('POST', `/api/activities/${id}/verify-hash`),
  registrations: (id) => request('GET', `/api/activities/${id}/registrations`),
  shipments: (id) => request('GET', `/api/activities/${id}/shipments`),
  audit: (id) => request('GET', `/api/activities/${id}/audit`),
  prizes: (id) => request('GET', `/api/activities/${id}/prizes`),

  register: (id, b) => request('POST', `/api/activities/${id}/register`, b),
  returnOrder: (id, b) => request('POST', `/api/activities/${id}/return`, b),
  review: (id, b) => request('POST', `/api/activities/${id}/review`, b),
  expire: (id) => request('POST', `/api/activities/${id}/expire`),

  viewer: (id, account) => request('GET', `/api/activities/${id}/viewer/${encodeURIComponent(account)}`),
  confirmWin: (id, account, b) =>
    request('POST', `/api/activities/${id}/winners/${encodeURIComponent(account)}/confirm`, b),
  ship: (id, b) => request('POST', `/api/activities/${id}/ship`, b),
  receipt: (b) => request('POST', '/api/logistics/receipt', b),
  ledger: (pid) => request('GET', `/api/prizes/${pid}/ledger`),
  items: (pid) => request('GET', `/api/prizes/${pid}/items`),

  simDemo: () => request('POST', '/api/sim/demo'),
}
