import { api, userApi } from './client';
import type { Bot, WechatQRCode, WechatQRCodeStatus } from './types';

export async function createWechatQRCode(): Promise<WechatQRCode> {
  return userApi.post<WechatQRCode>('/v1/users/me/wechat-qrcode');
}

export async function getWechatQRCodeStatus(qrcodeToken: string): Promise<WechatQRCodeStatus> {
  return userApi.get<WechatQRCodeStatus>(`/v1/users/me/wechat-qrcode/status?qrcode_token=${encodeURIComponent(qrcodeToken)}`);
}

export async function createAdminWechatQRCode(): Promise<WechatQRCode> {
  return api.post<WechatQRCode>('/v1/admin/me/wechat-qrcode');
}

export async function getAdminWechatQRCodeStatus(qrcodeToken: string): Promise<WechatQRCodeStatus> {
  return api.get<WechatQRCodeStatus>(`/v1/admin/me/wechat-qrcode/status?qrcode_token=${encodeURIComponent(qrcodeToken)}`);
}

export async function getOwnBot(): Promise<Bot> {
  return userApi.get<Bot>('/v1/users/me/bots');
}
