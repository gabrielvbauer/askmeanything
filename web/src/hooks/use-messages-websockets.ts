import { useEffect } from "react"
import type { GetRoomMessagesResponse } from "../http/get-room-messages"
import { useQueryClient } from "@tanstack/react-query"

interface UseMessagesWebsocketsParams {
  roomId: string
}

type WebhookMessage = 
  | { kind: 'message_created'; value: { id: string, message: string } }
  | { kind: 'message_answered'; value: { id: string } }
  | { kind: 'message_reaction_increased'; value: { id: string, count: number } }
  | { kind: 'message_reaction_decreased'; value: { id: string, count: number } }

export function useMessagesWebsockets({
  roomId
}: UseMessagesWebsocketsParams) {
  const queryClient = useQueryClient()
  
  useEffect(() => {
    const ws = new WebSocket(`ws://localhost:8080/subscribe/${roomId}`)

    ws.onopen = () => {
      console.log('Websocket connected!')
    }

    ws.onclose = () => {
      console.log('Websocket closed!')
    }

    ws.onmessage = (event) => {
      const data: WebhookMessage = JSON.parse(event.data)

      switch(data.kind) {
        case 'message_created':
          queryClient.setQueryData<GetRoomMessagesResponse>(['messages', roomId], state => {
            return {
              messages: [
                ...(state?.messages ?? []),
                {
                  id: data.value.id,
                  text: data.value.message,
                  amountOfReactions: 0,
                  answered: false
                }
              ],
            }
          })
          break;
        case 'message_answered':
          queryClient.setQueryData<GetRoomMessagesResponse>(['messages', roomId], state => {
            if (!state) {
              return undefined
            }

            return {
              messages: state.messages.map(message => {
                if (message.id === data.value.id) {
                  return {
                    ...message,
                    answered: true
                  }
                }

                return message
              })
            }
          })
          break;
        case 'message_reaction_increased':
        case 'message_reaction_decreased':
          queryClient.setQueryData<GetRoomMessagesResponse>(['messages', roomId], state => {
            if (!state) {
              return undefined
            }

            return {
              messages: state.messages.map(message => {
                if (message.id === data.value.id) {
                  return {
                    ...message,
                    amountOfReactions: data.value.count
                  }
                }

                return message
              })
            }
          })
          break;
      }
    }

    return () => {
      ws.close()
    }
  }, [roomId, queryClient])
}