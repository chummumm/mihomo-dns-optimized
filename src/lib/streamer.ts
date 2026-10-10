import EventEmitter from 'eventemitter3'
import { type SetRequired } from 'type-fest'

export interface Config {
    bufferLength?: number
    retryInterval?: number
}

export class StreamReader<T> {
    protected EE = new EventEmitter()

    protected config: SetRequired<Config, 'bufferLength' | 'retryInterval'>

    protected innerBuffer: T[] = []

    protected url = ''

    protected connection: WebSocket | null = null

    private retryTimer: ReturnType<typeof setTimeout> | undefined

    private stopped = false

    constructor (config: Config) {
        this.config = Object.assign(
            {
                bufferLength: 0,
                retryInterval: 5000,
            },
            config,
        )
    }

    protected connectWebsocket = () => {
        if (!this.url || this.stopped) {
            return
        }

        const url = new URL(this.url)

        const socket = new WebSocket(url.toString())
        this.connection = socket
        socket.addEventListener('message', msg => {
            if (this.connection !== socket || this.stopped) return
            let data: T
            try {
                data = JSON.parse(msg.data)
            } catch (error) {
                this.EE.emit('error', error)
                return
            }
            this.EE.emit('data', [data])
            if (this.config.bufferLength > 0) {
                this.innerBuffer.push(data)
                if (this.innerBuffer.length > this.config.bufferLength) {
                    this.innerBuffer.splice(0, this.innerBuffer.length - this.config.bufferLength)
                }
            }
        })

        const reconnect = () => {
            if (this.connection !== socket || this.stopped) return
            this.connection = null
            socket.close()
            if (this.retryTimer === undefined) {
                this.retryTimer = setTimeout(() => {
                    this.retryTimer = undefined
                    this.connectWebsocket()
                }, this.config.retryInterval)
            }
        }
        socket.addEventListener('error', err => {
            if (this.connection !== socket || this.stopped) return
            this.EE.emit('error', err)
            reconnect()
        })
        socket.addEventListener('close', reconnect)
    }

    connect (url: string) {
        if (this.url === url && this.connection && this.connection.readyState < WebSocket.CLOSING) {
            return
        }
        this.url = url
        this.stopped = false
        clearTimeout(this.retryTimer)
        this.retryTimer = undefined
        this.innerBuffer = []
        const previous = this.connection
        this.connection = null
        previous?.close()
        this.connectWebsocket()
    }

    subscribe (event: string, callback: (data: T[]) => void) {
        this.EE.addListener(event, callback)
    }

    unsubscribe (event: string, callback: (data: T[]) => void) {
        this.EE.removeListener(event, callback)
    }

    buffer () {
        return this.innerBuffer.slice()
    }

    destory () {
        this.stopped = true
        clearTimeout(this.retryTimer)
        this.retryTimer = undefined
        this.EE.removeAllListeners()
        const previous = this.connection
        this.connection = null
        previous?.close()
        this.innerBuffer = []
    }
}
