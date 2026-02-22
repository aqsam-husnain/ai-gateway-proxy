import { OAuth2Client } from 'google-auth-library';
import * as http from 'http';
import * as https from 'https';
import { promises as fs } from 'fs';
import * as path from 'path';
import * as os from 'os';
import * as readline from 'readline';
import { v4 as uuidv4 } from 'uuid';
import open from 'open';
import { formatExpiryTime } from '../common.js';
import { getProviderModels } from '../provider-models.js';
import { handleGeminiAntigravityOAuth } from '../oauth-handlers.js';

// Configure HTTP/HTTPS agent to limit connection pool size to avoid resource leaks
const httpAgent = new http.Agent({
    keepAlive: true,
    maxSockets: 100,
    maxFreeSockets: 5,
    timeout: 120000,
});
const httpsAgent = new https.Agent({
    keepAlive: true,
    maxSockets: 100,
    maxFreeSockets: 5,
    timeout: 120000,
});

// --- Constants ---
const CREDENTIALS_DIR = '.antigravity';
const CREDENTIALS_FILE = 'oauth_creds.json';
const DEFAULT_ANTIGRAVITY_BASE_URL_DAILY = 'https://daily-cloudcode-pa.sandbox.googleapis.com';
const DEFAULT_ANTIGRAVITY_BASE_URL_AUTOPUSH = 'https://autopush-cloudcode-pa.sandbox.googleapis.com';
const ANTIGRAVITY_API_VERSION = 'v1internal';
const OAUTH_CLIENT_ID = process.env.GEMINI_ANTIGRAVITY_OAUTH_CLIENT_ID;
const OAUTH_CLIENT_SECRET = process.env.GEMINI_ANTIGRAVITY_OAUTH_CLIENT_SECRET;
const DEFAULT_USER_AGENT = 'antigravity/1.11.5 windows/amd64';
const REFRESH_SKEW = 3000; // 3000 seconds (50 minutes) advance Token refresh

// Get Antigravity model list
const ANTIGRAVITY_MODELS = getProviderModels('gemini-antigravity');

// Model alias mapping (user-facing name -> API name)
const MODEL_ALIAS_MAP = {
    'gemini-2.5-computer-use-preview-10-2025': 'rev19-uic3-1p',
    'gemini-3-pro-image-preview': 'gemini-3-pro-image',
    'gemini-3-pro-preview': 'gemini-3-pro-high',
    'gemini-3-pro-low': 'gemini-3-pro-low',
    'gemini-3-flash-preview': 'gemini-3-flash',
    'gemini-2.5-flash': 'gemini-2.5-flash',
    'gemini-2.5-pro': 'gemini-2.5-pro',
    'gemini-2.5-flash-lite': 'gemini-2.5-flash-lite',
    'gemini-2.5-flash-thinking': 'gemini-2.5-flash-thinking',
    'gpt-oss-120b-medium': 'gpt-oss-120b-medium',
    'gemini-claude-sonnet-4-5': 'claude-sonnet-4-5',
    'gemini-claude-sonnet-4-5-thinking': 'claude-sonnet-4-5-thinking',
    'gemini-claude-opus-4-5-thinking': 'claude-opus-4-5-thinking'
};

// Model name mapping (API name -> user-facing name)
const MODEL_NAME_MAP = {
    'rev19-uic3-1p': 'gemini-2.5-computer-use-preview-10-2025',
    'gemini-3-pro-image': 'gemini-3-pro-image-preview',
    'gemini-3-pro-high': 'gemini-3-pro-preview',
    'gemini-3-pro-low': 'gemini-3-pro-low',
    'gemini-3-flash': 'gemini-3-flash-preview',
    'gemini-2.5-flash': 'gemini-2.5-flash',
    'gemini-2.5-pro': 'gemini-2.5-pro',
    'gemini-2.5-flash-lite': 'gemini-2.5-flash-lite',
    'gemini-2.5-flash-thinking': 'gemini-2.5-flash-thinking',
    'gpt-oss-120b-medium': 'gpt-oss-120b-medium',
    'claude-sonnet-4-5': 'gemini-claude-sonnet-4-5',
    'claude-sonnet-4-5-thinking': 'gemini-claude-sonnet-4-5-thinking',
    'claude-opus-4-5-thinking': 'gemini-claude-opus-4-5-thinking'
};


    async getUsageLimits() {
        if (!this.isInitialized) await this.initialize();
        
        // Check if token is near expiry, if so refresh first
        if (this.isExpiryDateNear()) {
            console.log('[Antigravity] Token is near expiry, refreshing before getUsageLimits request...');
            await this.initializeAuth(true);
        }

        try {
            const modelsWithQuotas = await this.getModelsWithQuotas();
            return modelsWithQuotas;
        } catch (error) {
            console.error('[Antigravity] Failed to get usage limits:', error.message);
            throw error;
        }
    }

    /**
     * Get model list with quota information
     * @returns {Promise<Object>} Model quota information
     */
    async getModelsWithQuotas() {
        try {
            // Parse model quota information
            const result = {
                lastUpdated: Date.now(),
                models: {}
            };

            // Call fetchAvailableModels API to get model and quota information
            for (const baseURL of this.baseURLs) {
                try {
                    const modelsURL = `${baseURL}/${ANTIGRAVITY_API_VERSION}:fetchAvailableModels`;
                    const requestOptions = {
                        url: modelsURL,
                        method: 'POST',
                        headers: {
                            'Content-Type': 'application/json',
                            'User-Agent': this.userAgent
                        },
                        responseType: 'json',
                        body: JSON.stringify({})
                    };

                    const res = await this.authClient.request(requestOptions);
                    console.log(`[Antigravity] fetchAvailableModels success`);
                    if (res.data && res.data.models) {
                        const modelsData = res.data.models;
                        
                        // Iterate through model data to extract quota information
                        for (const [modelId, modelData] of Object.entries(modelsData)) {
                            const aliasName = modelName2Alias(modelId);
                            if (aliasName == null ||aliasName === '') continue; // Skip unsupported models
                            
                            const modelInfo = {
                                remaining: 0,
                                resetTime: null,
                                resetTimeRaw: null
                            };
                            
                            // Extract quota information from quotaInfo
                            if (modelData.quotaInfo) {
                                modelInfo.remaining = modelData.quotaInfo.remainingFraction || modelData.quotaInfo.remaining || 0;
                                modelInfo.resetTime = modelData.quotaInfo.resetTime || null;
                                modelInfo.resetTimeRaw = modelData.quotaInfo.resetTime;
                            }
                            
                            result.models[aliasName] = modelInfo;
                        }

                        // Add any missing models from ANTIGRAVITY_MODELS with default values
                        // Respect notSupportedModels config - don't add models that are disabled
                        const notSupported = this.config?.notSupportedModels || [];
                        for (const modelId of ANTIGRAVITY_MODELS) {
                            if (!result.models[modelId] && !notSupported.includes(modelId)) {
                                result.models[modelId] = {
                                    remaining: 1, // 100% remaining (no usage)
                                    resetTime: null,
                                    resetTimeRaw: null
                                };
                            }
                        }

                        // Also remove any models from API response that are in notSupportedModels
                        for (const modelId of notSupported) {
                            delete result.models[modelId];
                        }

                        // Sort models by name
                        const sortedModels = {};
                        Object.keys(result.models).sort().forEach(key => {
                            sortedModels[key] = result.models[key];
                        });
                        result.models = sortedModels;
                        console.log(`[Antigravity] Successfully fetched quotas for ${Object.keys(result.models).length} models`);
                        break; // Exit loop after successful retrieval
                    }
                } catch (error) {
                    console.error(`[Antigravity] Failed to fetch models with quotas from ${baseURL}:`, error.message);
                }
            }

            return result;
        } catch (error) {
            console.error('[Antigravity] Failed to get models with quotas:', error.message);
            throw error;
        }
    }

}