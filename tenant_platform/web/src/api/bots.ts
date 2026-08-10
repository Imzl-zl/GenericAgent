import { api, userApi } from './client';
import type { Bot, ChannelBinding, SaveChannelBindingRequest, WechatQRCode, WechatQRCodeStatus, ChannelType } from './types';

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

// 渠道绑定(IM_CHANNEL_BINDING §4): user + admin 同款路径。
export async function listBindings(): Promise<ChannelBinding[]> {
  return userApi.get<ChannelBinding[]>('/v1/me/im-bindings');
}

export async function saveBinding(channelType: ChannelType, body: SaveChannelBindingRequest): Promise<ChannelBinding> {
  return userApi.put<ChannelBinding>(`/v1/me/im-bindings/${channelType}`, body);
}

export async function unbindChannel(channelType: ChannelType): Promise<ChannelBinding> {
  return userApi.delete<ChannelBinding>(`/v1/me/im-bindings/${channelType}`);
}

export async function listAdminBindings(): Promise<ChannelBinding[]> {
  return api.get<ChannelBinding[]>('/v1/admin/me/im-bindings');
}

export async function saveAdminBinding(channelType: ChannelType, body: SaveChannelBindingRequest): Promise<ChannelBinding> {
  return api.put<ChannelBinding>(`/v1/admin/me/im-bindings/${channelType}`, body);
}

export async function unbindAdminChannel(channelType: ChannelType): Promise<ChannelBinding> {
  return api.delete<ChannelBinding>(`/v1/admin/me/im-bindings/${channelType}`);
}
