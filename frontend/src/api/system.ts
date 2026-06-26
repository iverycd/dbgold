import api from './index'

export interface VersionInfo {
  version: string
  git_commit: string
  build_time: string
}

export const getVersion = () => api.get<VersionInfo>('/version')
